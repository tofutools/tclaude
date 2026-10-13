// tclaude-pi-permissions: tclaude's human-gated `ask` approval posture for
// the pi coding agent.
//
// pi itself never asks before a tool call. Under `ask`, tclaude loads this
// file next to its status extension, read-only and per launch with
// `pi -e <path>`, and it asks the human before every tool call that is not
// read-only. A call the human declines is blocked, and the model sees why.
//
// Rules this file keeps:
//   - It fails closed. A call it cannot classify asks; with no human to ask
//     (print or JSON mode) it is blocked; and pi itself blocks a tool when a
//     tool_call handler throws.
//   - Read-only means pi's read-only built-ins, or a tool whose annotations
//     say it is read-only (MCP servers declare them), as pi's own permission
//     example reads them.
//
// Keep it dependency-free: pi loads it with jiti, outside any project.

export const TCLAUDE_PI_PERMISSIONS_VERSION = 1;

// pi's built-in tools that only read.
const READ_ONLY_TOOLS = new Set(["read", "grep", "find", "ls"]);

// How much of a call's arguments the dialog shows; the rest is elided from
// the middle, with a count, so neither end can be pushed out of view.
const MAX_SUMMARY_CHARS = 1200;

function readOnly(pi: any, name: string): boolean {
	if (READ_ONLY_TOOLS.has(name)) return true;
	const tools: any[] = pi.getAllTools?.() ?? [];
	const hints = tools.find((tool) => tool?.name === name)?.annotations;
	return hints?.readOnlyHint === true && hints?.destructiveHint !== true;
}

// visible makes model-controlled text safe to show in the terminal: control
// characters (ESC sequences could hide or redraw what the human reads), C1
// controls and bidirectional overrides are shown as escapes, never obeyed.
function visible(text: string): string {
	return text.replace(/[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u200e\u200f\u202a-\u202e\u2066-\u2069]/g, (c) =>
		"\\u" + c.charCodeAt(0).toString(16).padStart(4, "0"),
	);
}

// summary is what the human approves: the command or path first, then every
// other argument, so a file's new content or a tool's other fields are shown
// too.
function summary(input: any): string {
	const rest: Record<string, unknown> = { ...(input && typeof input === "object" ? input : {}) };
	const lead: string[] = [];
	for (const key of ["command", "path"]) {
		if (typeof rest[key] === "string") {
			lead.push(rest[key] as string);
			delete rest[key];
		}
	}
	let text = lead.join("\n");
	if (Object.keys(rest).length > 0) {
		let args: string;
		try {
			args = JSON.stringify(rest, null, 1);
		} catch {
			args = "(arguments that cannot be shown)";
		}
		text = text ? text + "\n" + args : args;
	}
	text = visible(text);
	if (text.length <= MAX_SUMMARY_CHARS) return text;
	const keep = Math.floor(MAX_SUMMARY_CHARS / 2);
	const omitted = text.length - 2 * keep;
	return `${text.slice(0, keep)}\n… ${omitted} characters not shown …\n${text.slice(-keep)}`;
}

// One dialog at a time: pi replaces an open dialog with a newer one without
// answering it, which would leave the first call waiting forever.
let dialogs: Promise<unknown> = Promise.resolve();

function ask(ctx: any, title: string, message: string): Promise<boolean> {
	const answer = dialogs.then(() => {
		if (ctx?.signal?.aborted) return false;
		return ctx.ui.confirm(title, message, { signal: ctx?.signal });
	});
	dialogs = answer.catch(() => false);
	return answer.then((approved) => approved === true).catch(() => false);
}

export default function tclaudePiPermissions(pi: any): void {
	pi.on("tool_call", async (event: any, ctx: any) => {
		const name = typeof event?.toolName === "string" ? event.toolName : "";
		if (name !== "" && readOnly(pi, name)) return;
		const label = name ? visible(name) : "an unnamed tool";
		if (!ctx?.hasUI) {
			return { block: true, reason: `tclaude's ask posture: ${label} needs a human's approval, and no human can answer in this mode` };
		}
		if (!(await ask(ctx, `Allow ${label}?`, summary(event?.input)))) {
			return { block: true, reason: `tclaude's ask posture: the human did not approve ${label}` };
		}
	});
}
