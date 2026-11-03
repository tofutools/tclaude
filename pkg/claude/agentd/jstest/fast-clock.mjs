// A scaled clock for suites that wait on real production timers.
//
// Some dashboard islands debounce, retry and poll on second-scale timers
// (the board's 3 s event retry, dictation's silence countdown), and their
// suites check them by sleeping that long for real. Each of those suites ran
// 50-80 s while its process sat almost idle, and set the jstest shard's
// critical path.
//
// installFastClock(scale) runs the whole suite file on a clock `1/scale`
// times faster: every setTimeout/setInterval delay is multiplied by `scale`,
// and Date.now(), new Date() and performance.now() advance at `1/scale` of
// real speed. Production timers and the tests' own waits shrink together, so
// their order and every "wait past X" margin keep their meaning, and no test
// has to be rewritten into explicit ticks. What does not scale is real CPU
// work: a render that takes 5 ms of real time takes `5/scale` ms of clock
// time, so keep `scale` large enough that ordinary work stays well inside a
// test's margins (the default 0.1 turns a 20 ms margin into 2 ms of real
// time, still far more than the microtask hops between a timer and its
// effect).
//
// Call it at the top of a suite file, before any test runs. node --test runs
// every file in its own process, so it affects that file only. It patches
// globals, and does not mix with t.mock.timers in the same file.
export function installFastClock(scale = 0.1) {
  if (!(scale > 0 && scale <= 1)) throw new RangeError(`fast clock scale ${scale} must be in (0, 1]`);
  if (globalThis.__tclaudeFastClock) throw new Error('installFastClock called twice');

  const real = {
    setTimeout: globalThis.setTimeout,
    setInterval: globalThis.setInterval,
    Date: globalThis.Date,
    performanceNow: performance.now.bind(performance),
  };
  const realStart = real.performanceNow();
  const dateStart = real.Date.now();
  const elapsed = () => (real.performanceNow() - realStart) / scale;
  const scaled = (ms) => {
    const delay = Number(ms);
    return Number.isFinite(delay) && delay > 0 ? delay * scale : 0;
  };

  globalThis.setTimeout = Object.assign(
    (fn, ms, ...args) => real.setTimeout(fn, scaled(ms), ...args),
    real.setTimeout,
  );
  globalThis.setInterval = Object.assign(
    (fn, ms, ...args) => real.setInterval(fn, scaled(ms), ...args),
    real.setInterval,
  );

  const RealDate = real.Date;
  const now = () => Math.floor(dateStart + elapsed());
  // A subclass keeps Date.parse, Date.UTC, instanceof and every method; only
  // "now" (no-argument construction, Date(), Date.now) reads the fast clock.
  function FastDate(...args) {
    if (!new.target) return new RealDate(now()).toString();
    return Reflect.construct(RealDate, args.length ? args : [now()], new.target);
  }
  Object.setPrototypeOf(FastDate, RealDate);
  FastDate.prototype = RealDate.prototype;
  FastDate.now = now;
  globalThis.Date = FastDate;

  performance.now = () => realStart + elapsed();

  globalThis.__tclaudeFastClock = { scale };
  return { scale };
}
