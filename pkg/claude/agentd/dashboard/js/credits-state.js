import { batch, computed, signal } from '@preact/signals';

export const CREDIT_PAYOUTS = Object.freeze({
  'win-idle': 100,
  'win-pull': 50,
  'win-spawn': 25,
  'win-mega': 777,
});

export const CREDIT_HOT_WINDOW_MS = 60 * 1000;
export const CREDIT_HOT_THRESHOLD = 3;
export const CREDIT_LEADERBOARD_MAX = 8;

// Credits are deliberately session-only. This state owns the bookkeeping but
// knows nothing about the DOM, the event bus, or dashboard rendering; those
// lifecycle boundaries live in credits-island.js and slop-credits.js.
export function createCreditsState({ now = () => Date.now() } = {}) {
  const credits = signal(0);
  const bumpVersion = signal(0);
  const winsByConv = signal(new Map());
  const winTimesByConv = signal(new Map());
  const snapshot = signal(null);
  const observedAt = signal(now());

  const entries = computed(() => {
    const value = snapshot.value;
    const cutoff = observedAt.value - CREDIT_HOT_WINDOW_MS;
    return Array.from(winsByConv.value.entries())
      .sort((a, b) => b[1] - a[1])
      .slice(0, CREDIT_LEADERBOARD_MAX)
      .map(([conv, wins], index) => {
        const agent = (value?.agents || []).find((candidate) => candidate.conv_id === conv);
        const recent = winTimesByConv.value.get(conv) || [];
        return {
          conv,
          wins,
          rank: index + 1,
          title: agent?.title || conv.slice(0, 8),
          hot: recent.filter((time) => time >= cutoff).length >= CREDIT_HOT_THRESHOLD,
        };
      });
  });

  const view = computed(() => ({
    credits: credits.value,
    bumpVersion: bumpVersion.value,
    entries: entries.value,
  }));

  function publishSnapshot(value) {
    batch(() => {
      snapshot.value = value || null;
      // A snapshot tick also advances hot-streak decay even when no new win was
      // recorded. The clock is injected so the rule stays deterministic in tests.
      observedAt.value = now();
    });
  }

  function recordWin(fx, conv = '') {
    const payout = CREDIT_PAYOUTS[fx];
    if (!payout) return { accepted: false, attributed: false, payout: 0 };

    const attributed = !!conv && (fx === 'win-idle' || fx === 'win-pull');
    batch(() => {
      credits.value += payout;
      bumpVersion.value += 1;
      observedAt.value = now();
      if (!attributed) return;

      const wins = new Map(winsByConv.value);
      wins.set(conv, (wins.get(conv) || 0) + 1);
      winsByConv.value = wins;

      const cutoff = observedAt.value - CREDIT_HOT_WINDOW_MS;
      const times = new Map(winTimesByConv.value);
      times.set(conv, [...(times.get(conv) || []), observedAt.value]
        .filter((time) => time >= cutoff));
      winTimesByConv.value = times;
    });
    return { accepted: true, attributed, payout };
  }

  return Object.freeze({
    credits,
    bumpVersion,
    winsByConv,
    winTimesByConv,
    snapshot,
    observedAt,
    entries,
    view,
    publishSnapshot,
    recordWin,
  });
}

export const creditsState = createCreditsState();
