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
//
// Keep it dependency-free: pi loads it with jiti, outside any project.

import { spawn } from "node:child_process";

export const TCLAUDE_PI_EXTENSION_VERSION = 1;

const HOOK_BIN = process.env.TCLAUDE_PI_HOOK_BIN ?? "";
const CALLBACK_TIMEOUT_MS = 5_000;
// How long shutdown waits for earlier events before sending SessionEnd.
const SHUTDOWN_FLUSH_MS = 3_000;
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
	const model = ctx?.model;
	if (model?.provider && model?.id) payload.model = `${model.provider}/${model.id}`;
	return payload;
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
	return reason === "manual" ? "manual" : "auto";
}

export default function tclaudePi(pi: any): void {
	pi.on("session_start", (event: any, ctx: any) => {
		emit("SessionStart", ctx, { source: START_SOURCE[event?.reason] ?? "startup" });
	});

	pi.on("input", (event: any, ctx: any) => {
		// A slash command is pi's own, not a prompt to the model.
		const text = typeof event?.text === "string" ? event.text : "";
		if (text.trimStart().startsWith("/")) return;
		emit("UserPromptSubmit", ctx, { prompt: text });
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
	});

	pi.on("session_before_compact", async (event: any, ctx: any) => {
		const reason = event?.reason ?? "manual";
		const out = await emit("PreCompact", ctx, { trigger: compactTrigger(reason) });
		// tclaude may refuse an early automatic compaction. An overflow
		// compaction is never refused: without it the turn cannot continue.
		if (reason !== "threshold") return;
		try {
			const decision = JSON.parse(out.trim() || "{}");
			if (decision?.decision === "block") return { cancel: true };
		} catch {}
	});

	pi.on("session_compact", (event: any, ctx: any) => {
		emit("PostCompact", ctx, { trigger: compactTrigger(event?.reason ?? "manual") });
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
		if (reason === "quit") closed = true;
		await end;
	});
}
