// tclaude-pi: tclaude's status bridge for the pi coding agent.
//
// tclaude writes this file read-only and loads it per launch with
// `pi -e <path>`; it is never installed into ~/.pi. It turns pi's extension
// events into the Claude Code-shaped payloads `tclaude session hook-callback`
// already understands, so tclaude's shared hook path needs no pi branch.
//
// Rules this file keeps:
//   - It never breaks pi. Every handler catches everything; a missing,
//     slow or failing tclaude costs status updates, never the session.
//   - It runs the callback without a shell, from an absolute path tclaude
//     put in TCLAUDE_PI_HOOK_BIN, with a bounded payload and a timeout.
//   - Events are delivered one at a time and in order. Only PreCompact
//     (whose answer can cancel a compaction) and shutdown (which must flush
//     before pi exits) wait for delivery; everything else is queued so a
//     turn is never slowed by tclaude.
//   - The drive (an agent spawned with --pi-drive) asks tclaude's daemon,
//     over its agent socket, what to deliver, and runs it through pi's own
//     API: a fixed set of operations, never commands. The daemon answers
//     "no drive" for every other launch, and the loop stops.
//
// Keep it dependency-free: pi loads it with jiti, outside any project.

import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { request } from "node:http";
import { homedir } from "node:os";
import { isAbsolute, join } from "node:path";

export const TCLAUDE_PI_EXTENSION_VERSION = 4;

const HOOK_BIN = process.env.TCLAUDE_PI_HOOK_BIN ?? "";
// tclaude's auto-compaction window in tokens, when the agent pins one. pi
// compacts on its own only near the model's full window; this compacts
// earlier, between runs. pi's own threshold stays the backstop.
// tclaude exports a canonical token count; anything else (a unit suffix from
// the operator's own shell, a value outside tclaude's bounds) is ignored, as
// tclaude's hook ignores it, so the compaction and the meter agree.
const AUTO_COMPACT_TOKENS = pinnedWindow(process.env.CLAUDE_CODE_AUTO_COMPACT_WINDOW ?? "");

function pinnedWindow(raw: string): number {
	const value = raw.trim();
	if (!/^[0-9]+$/.test(value)) return 0;
	const tokens = Number(value);
	return tokens >= 10_000 && tokens <= 10_000_000 ? tokens : 0;
}
// A managed launch's rules (RULES.md, which tclaude wrote read-only for this
// launch), appended to the system prompt on every turn, so they survive a
// compaction. pi's own --append-system-prompt would switch off its discovery
// of the operator's APPEND_SYSTEM.md, so tclaude uses this instead.
const MANAGED_RULES = managedRules(process.env.TCLAUDE_PI_MANAGED_RULES ?? "");

function managedRules(path: string): string {
	if (!path) return "";
	try {
		return readFileSync(path, "utf8").trim();
	} catch {
		return "";
	}
}
const CALLBACK_TIMEOUT_MS = 5_000;
// How long shutdown waits for earlier events before sending SessionEnd.
const SHUTDOWN_FLUSH_MS = 3_000;
// How long a threshold compaction waits for tclaude's verdict before going
// ahead: the answer is advice, never a reason to stall the turn.
const PRECOMPACT_DECISION_MS = 2_000;
const MAX_FIELD_BYTES = 32 * 1024;
const MAX_RESPONSE_BYTES = 64 * 1024;

type Payload = Record<string, unknown>;

let queue: Promise<unknown> = Promise.resolve();
let warned = false;
// closed is set once SessionEnd is sent: a queued event that has not started
// by then is dropped, so a stuck tclaude cannot keep pi from exiting.
let closed = false;

function warn(ctx: any, message: string): void {
	if (warned) return;
	warned = true;
	try {
		ctx?.ui?.notify?.(`tclaude: ${message}; live status is unavailable for this session`, "warning");
	} catch {
		// Nothing left to report through.
	}
	showStatus(ctx);
}

// The key of tclaude's segment in pi's footer.
const STATUS_KEY = "tclaude";

// showStatus renders tclaude's segment in pi's footer: that tclaude manages
// the pane, and, when the agent pins an auto-compaction window, the context
// measured against it, as tclaude's dashboard meter and status bar measure it
// (pi's own footer measures the model's full window).
function showStatus(ctx: any): void {
	try {
		if (!ctx?.hasUI) return;
		let text = "tclaude";
		if (!HOOK_BIN || warned) {
			text += " · no live status";
		} else if (AUTO_COMPACT_TOKENS > 0) {
			const tokens = ctx?.getContextUsage?.()?.tokens;
			if (typeof tokens === "number" && tokens >= 0) {
				const percent = Math.round((tokens * 100) / AUTO_COMPACT_TOKENS);
				text += ` · ctx ${percent}% of ${Math.round(AUTO_COMPACT_TOKENS / 1000)}k`;
			}
		}
		ctx.ui.setStatus(STATUS_KEY, text);
	} catch {
		// The segment is cosmetic.
	}
}

// deliver runs one hook-callback and resolves to its stdout ("" on any
// failure). It never rejects.
function deliver(payload: Payload): Promise<string> {
	if (closed) return Promise.resolve("");
	return new Promise((resolve) => {
		let out = "";
		let done = false;
		const finish = () => {
			if (!done) {
				done = true;
				resolve(out);
			}
		};
		try {
			// Its own process group, so a timeout can kill anything the
			// callback started: a surviving grandchild holding the stdout
			// pipe would otherwise keep pi's event loop, and pi, alive.
			const child = spawn(HOOK_BIN, ["session", "hook-callback"], {
				stdio: ["pipe", "pipe", "ignore"],
				env: process.env,
				shell: false,
				detached: true,
				windowsHide: true,
			});
			const timer = setTimeout(() => {
				try {
					if (child.pid) process.kill(-child.pid, "SIGKILL");
				} catch {}
				try {
					child.kill("SIGKILL");
				} catch {}
				child.stdout?.destroy();
				child.stdin?.destroy();
				child.unref();
				finish();
			}, CALLBACK_TIMEOUT_MS);
			child.stdout?.on("data", (chunk: Buffer) => {
				if (out.length < MAX_RESPONSE_BYTES) out += chunk.toString("utf8");
			});
			child.on("error", () => {
				clearTimeout(timer);
				finish();
			});
			child.on("close", () => {
				clearTimeout(timer);
				finish();
			});
			child.stdin?.on("error", () => {});
			child.stdin?.end(JSON.stringify(payload) + "\n");
		} catch {
			finish();
		}
	});
}

// within resolves to p's value, or to "" once ms have passed.
function within(p: Promise<string>, ms: number): Promise<string> {
	let timer: ReturnType<typeof setTimeout> | undefined;
	const late = new Promise<string>((resolve) => {
		timer = setTimeout(() => resolve(""), ms);
	});
	return Promise.race([p, late]).finally(() => {
		if (timer) clearTimeout(timer);
	});
}

// send queues a payload behind every earlier one and resolves to its stdout.
function send(payload: Payload): Promise<string> {
	const next = queue.then(() => deliver(payload));
	queue = next.catch(() => "");
	return next.catch(() => "");
}

// bounded returns value, or undefined plus a trimmed flag when its JSON form
// exceeds MAX_FIELD_BYTES.
function bounded(value: unknown, trimmed: { flag: boolean }): unknown {
	if (value === undefined) return undefined;
	try {
		const raw = typeof value === "string" ? value : JSON.stringify(value);
		if (Buffer.byteLength(raw ?? "", "utf8") <= MAX_FIELD_BYTES) return value;
	} catch {}
	trimmed.flag = true;
	return undefined;
}

// base is the identity every payload carries: pi's session id is tclaude's
// conversation id, and its session file is the transcript.
function base(event: string, ctx: any): Payload {
	const sm = ctx?.sessionManager;
	const payload: Payload = {
		session_id: sm?.getSessionId?.() ?? "",
		transcript_path: sm?.getSessionFile?.() ?? "",
		cwd: ctx?.cwd ?? process.cwd(),
		hook_event_name: event,
	};
	try {
		const usage = ctx?.getContextUsage?.();
		if (usage) {
			payload.tclaude_context = {
				tokens: usage.tokens ?? null,
				context_window: usage.contextWindow ?? null,
				percent: usage.percent ?? null,
			};
		}
	} catch {}
	try {
		const spend = sessionSpend(ctx);
		if (spend) payload.tclaude_usage = spend;
	} catch {}
	const model = ctx?.model;
	if (model?.provider && model?.id) payload.model = `${model.provider}/${model.id}`;
	return payload;
}

// sessionSpend sums what this session itself spent: every usage pi recorded
// (responses, compaction and branch summaries, cache warming) at or after the
// session's own start. A session made by /clone or /fork, or landed by
// tclaude, starts with copies of earlier entries whose spend belongs to the
// session they came from. The cost is pi's, priced from its model catalog; a
// subscription login is not billed per token, so the figure is then only what
// it would have cost.
function sessionSpend(ctx: any): Payload | undefined {
	const sm = ctx?.sessionManager;
	const started = Date.parse(sm?.getHeader?.()?.timestamp ?? "");
	const entries: any[] = sm?.getEntries?.() ?? [];
	let input = 0;
	let output = 0;
	let cost = 0;
	let calls = 0;
	for (const entry of entries) {
		if (Number.isFinite(started) && Date.parse(entry?.timestamp ?? "") < started) continue;
		const usage = entry?.type === "message" ? entry?.message?.usage : entry?.usage;
		if (!usage || typeof usage !== "object") continue;
		calls++;
		input += (usage.input ?? 0) + (usage.cacheRead ?? 0) + (usage.cacheWrite ?? 0);
		output += usage.output ?? 0;
		cost += usage.cost?.total ?? 0;
	}
	if (calls === 0) return undefined;
	let subscription = false;
	try {
		subscription = ctx?.model ? ctx?.modelRegistry?.isUsingOAuth?.(ctx.model) === true : false;
	} catch {}
	return { input_tokens: input, output_tokens: output, cost_usd: cost, calls, subscription };
}

function emit(event: string, ctx: any, fields: Payload = {}): Promise<string> {
	return build(event, ctx, fields, send);
}

// emitNow delivers without queueing, for SessionEnd after its bounded flush.
function emitNow(event: string, ctx: any, fields: Payload = {}): Promise<string> {
	return build(event, ctx, fields, deliver);
}

function build(event: string, ctx: any, fields: Payload, via: (p: Payload) => Promise<string>): Promise<string> {
	try {
		if (!HOOK_BIN) {
			warn(ctx, "TCLAUDE_PI_HOOK_BIN is not set");
			return Promise.resolve("");
		}
		const payload = { ...base(event, ctx) };
		const trimmed = { flag: false };
		for (const [key, value] of Object.entries(fields)) {
			const kept = bounded(value, trimmed);
			if (kept !== undefined) payload[key] = kept;
		}
		if (trimmed.flag) payload.tclaude_payload_trimmed = true;
		return via(payload);
	} catch {
		return Promise.resolve("");
	}
}

// The drive: a long poll on the daemon's agent socket. Each answer carries the
// requests queued for this conversation; their results go back on the next
// poll. The daemon tells a launch without the drive to stop.
const DRIVE_PATH = "/v1/whoami/pi-drive";
const DRIVE_POLL_TIMEOUT_MS = 60_000;
const DRIVE_BACKOFF_MAX_MS = 30_000;
const MAX_DRIVE_RESPONSE_BYTES = 4 * 1024 * 1024;

function driveSocket(): string {
	const explicit = (process.env.TCLAUDE_AGENTD_SOCKET ?? "").trim();
	if (explicit && isAbsolute(explicit)) return explicit;
	return join(homedir(), ".tclaude", "api", "agentd-socket", "agentd.sock");
}

type DriveAck = { id: number; ok: boolean; error?: string };

// One drive per pi process, whichever module instance started it; ctx is the
// latest session's, since pi may load this module afresh per session.
const drive: { started: boolean; stopped: boolean; ctx: any; pi: any; acks: DriveAck[]; req: any } =
	((globalThis as any).__tclaudePiDrive ??= { started: false, stopped: false, ctx: undefined, pi: undefined, acks: [], req: undefined });

function drivePoll(): Promise<{ status: number; body: any }> {
	return new Promise((resolve) => {
		const acks = drive.acks;
		drive.acks = [];
		const data = JSON.stringify({ acks });
		let settled = false;
		const finish = (status: number, body: any) => {
			if (settled) return;
			settled = true;
			drive.req = undefined;
			if (status !== 200) drive.acks = acks.concat(drive.acks);
			resolve({ status, body });
		};
		try {
			const req = request(
				{
					socketPath: driveSocket(),
					path: DRIVE_PATH,
					method: "POST",
					headers: { "content-type": "application/json", "content-length": Buffer.byteLength(data) },
					timeout: DRIVE_POLL_TIMEOUT_MS,
				},
				(res) => {
					let raw = "";
					res.setEncoding("utf8");
					res.on("data", (chunk: string) => {
						raw += chunk;
						if (raw.length > MAX_DRIVE_RESPONSE_BYTES) res.destroy();
					});
					res.on("end", () => {
						let body: any = undefined;
						try {
							body = JSON.parse(raw);
						} catch {}
						finish(res.statusCode ?? 0, body);
					});
					res.on("error", () => finish(0, undefined));
				},
			);
			drive.req = req;
			// The poll never keeps pi alive on its own.
			req.on("socket", (socket: any) => socket?.unref?.());
			req.on("timeout", () => req.destroy());
			req.on("error", () => finish(0, undefined));
			req.end(data);
		} catch {
			finish(0, undefined);
		}
	});
}

function driveRun(op: any): DriveAck {
	const id = typeof op?.id === "number" ? op.id : 0;
	const ctx = drive.ctx;
	const pi = drive.pi;
	try {
		switch (op?.op) {
			case "message": {
				const text = typeof op.text === "string" ? op.text : "";
				const deliverAs = op.deliver === "followUp" ? "followUp" : "steer";
				// While pi works, a message joins the run as a steer or waits
				// for it as a follow-up; when idle it starts a turn.
				if (ctx?.isIdle?.() === false) pi.sendUserMessage(text, { deliverAs });
				else pi.sendUserMessage(text);
				break;
			}
			case "compact": {
				const followUp = typeof op.text === "string" ? op.text : "";
				const after = () => {
					if (!followUp) return;
					try {
						pi.sendUserMessage(followUp, { deliverAs: "followUp" });
					} catch {}
				};
				ctx.compact({ onComplete: after, onError: after });
				break;
			}
			case "rename":
				pi.setSessionName(typeof op.name === "string" ? op.name : "");
				break;
			case "exit":
				// ctx.shutdown() only takes effect when a turn settles, so an
				// idle pi would never act on it. SIGTERM is pi's own graceful
				// path: it ends any running turn, runs session_shutdown and
				// exits (status 0 in the TUI).
				drive.stopped = true;
				setImmediate(() => process.kill(process.pid, "SIGTERM"));
				break;
			default:
				return { id, ok: false, error: `unknown operation ${String(op?.op)}` };
		}
		return { id, ok: true };
	} catch (err: any) {
		return { id, ok: false, error: String(err?.message ?? err).slice(0, 500) };
	}
}

async function driveLoop(): Promise<void> {
	let backoff = 500;
	while (!drive.stopped) {
		const { status, body } = await drivePoll();
		if (drive.stopped) return;
		if (status === 200 && body && typeof body === "object") {
			if (body.drive !== true) {
				drive.stopped = true;
				return;
			}
			backoff = 500;
			for (const op of Array.isArray(body.requests) ? body.requests : []) {
				drive.acks.push(driveRun(op));
			}
			continue;
		}
		if (status === 401 || status === 403 || status === 404 || status === 405) {
			// Not this process's channel, or a daemon without the drive.
			drive.stopped = true;
			return;
		}
		await new Promise((resolve) => {
			const timer = setTimeout(resolve, backoff);
			(timer as any)?.unref?.();
		});
		backoff = Math.min(backoff * 2, DRIVE_BACKOFF_MAX_MS);
	}
}

function startDrive(pi: any, ctx: any): void {
	drive.pi = pi;
	drive.ctx = ctx;
	if (drive.started) return;
	drive.started = true;
	driveLoop().catch(() => {
		drive.stopped = true;
	});
}

// Claude Code's SessionStart sources. "clear" and "resume" tell tclaude the
// pane moved to another conversation (pi's /new, /clone, /fork, /resume).
const START_SOURCE: Record<string, string> = {
	startup: "startup",
	reload: "startup",
	new: "clear",
	fork: "clear",
	resume: "resume",
};

// Claude Code's SessionEnd reasons.
const END_REASON: Record<string, string> = {
	quit: "prompt_input_exit",
	new: "clear",
	fork: "clear",
	resume: "resume",
};

function compactTrigger(reason: string): string {
	return reason === "manual" && !pinnedCompaction.active ? "manual" : "auto";
}

// Set while a compaction this extension started at the pinned window runs.
// pi reports it as manual; to tclaude it is automatic, so the pre-compact
// guard judges it like pi's own threshold compaction.
const pinnedCompaction = { active: false };

// The session last announced. pi can deliver session_start twice for one
// session switch (observed in RPC mode), and may load this module afresh for
// the new session's runtime, so the record lives on globalThis.
const announced: { id: string } = ((globalThis as any).__tclaudePiAnnounced ??= { id: "" });

export default function tclaudePi(pi: any): void {
	pi.on("session_start", (event: any, ctx: any) => {
		let id = "";
		try {
			id = ctx?.sessionManager?.getSessionId?.() ?? "";
		} catch {}
		if (id !== "" && id === announced.id && event?.reason !== "startup") return;
		announced.id = id;
		emit("SessionStart", ctx, { source: START_SOURCE[event?.reason] ?? "startup" });
		showStatus(ctx);
		try {
			startDrive(pi, ctx);
		} catch {}
	});

	pi.on("before_agent_start", (event: any) => {
		if (!MANAGED_RULES) return;
		try {
			const options = event?.systemPromptOptions;
			if (!options) return;
			const appended = typeof options.appendSystemPrompt === "string" ? options.appendSystemPrompt.trim() : "";
			options.appendSystemPrompt = appended ? `${appended}\n\n${MANAGED_RULES}` : MANAGED_RULES;
		} catch {
			// The briefing still carries the rules.
		}
	});

	// pi's own commands (/name, /compact, /new, ...) never reach "input";
	// slash text that does (a prompt template, a skill, an unknown command) is
	// a real turn for the model.
	pi.on("input", (event: any, ctx: any) => {
		emit("UserPromptSubmit", ctx, { prompt: typeof event?.text === "string" ? event.text : "" });
	});

	pi.on("tool_execution_start", (event: any, ctx: any) => {
		emit("PreToolUse", ctx, { tool_name: event?.toolName ?? "", tool_input: event?.args ?? {} });
	});

	pi.on("tool_execution_end", (event: any, ctx: any) => {
		emit("PostToolUse", ctx, { tool_name: event?.toolName ?? "" });
	});

	// A blocking dialog: a confirmation is a permission question, any other
	// kind asks for input. Answering it is followed by the tool run or the
	// settled turn, which move the status on.
	pi.on("ui_prompt_start", (event: any, ctx: any) => {
		emit("Notification", ctx, {
			notification_type: event?.kind === "confirm" ? "permission_prompt" : "elicitation_dialog",
			message: typeof event?.title === "string" ? event.title : "pi is waiting for input",
		});
	});

	pi.on("agent_settled", (_event: any, ctx: any) => {
		emit("Stop", ctx);
		showStatus(ctx);
		if (AUTO_COMPACT_TOKENS > 0) {
			try {
				const tokens = ctx?.getContextUsage?.()?.tokens;
				if (typeof tokens === "number" && tokens >= AUTO_COMPACT_TOKENS && !pinnedCompaction.active) {
					pinnedCompaction.active = true;
					const done = () => {
						pinnedCompaction.active = false;
					};
					ctx?.compact?.({ onComplete: done, onError: done });
				}
			} catch {}
		}
	});

	pi.on("session_before_compact", async (event: any, ctx: any) => {
		const reason = event?.reason ?? "manual";
		const pending = emit("PreCompact", ctx, { trigger: compactTrigger(reason) });
		// tclaude may refuse an early automatic compaction, so only that one
		// waits for its answer, briefly. A manual compaction is the user's
		// own, and an overflow compaction must run for the turn to continue.
		if (reason !== "threshold" && !(reason === "manual" && pinnedCompaction.active)) return;
		const out = await within(pending, PRECOMPACT_DECISION_MS);
		try {
			const decision = JSON.parse(out.trim() || "{}");
			if (decision?.decision === "block") return { cancel: true };
		} catch {}
	});

	pi.on("session_compact", (event: any, ctx: any) => {
		emit("PostCompact", ctx, { trigger: compactTrigger(event?.reason ?? "manual") });
		showStatus(ctx);
	});

	pi.on("session_shutdown", async (event: any, ctx: any) => {
		const reason = event?.reason;
		if (reason === "reload") return;
		// Give earlier events a bounded chance to land in order, then send
		// SessionEnd itself and drop whatever has not started.
		let timer: ReturnType<typeof setTimeout> | undefined;
		await Promise.race([
			queue,
			new Promise((resolve) => {
				timer = setTimeout(resolve, SHUTDOWN_FLUSH_MS);
			}),
		]);
		if (timer) clearTimeout(timer);
		const end = emitNow("SessionEnd", ctx, { reason: END_REASON[reason] ?? "other" });
		if (reason === "quit") {
			closed = true;
			drive.stopped = true;
			try {
				drive.req?.destroy?.();
			} catch {}
		}
		await end;
	});
}
