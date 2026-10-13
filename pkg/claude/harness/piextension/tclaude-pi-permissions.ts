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

// How much of a call's arguments the dialog shows.
const MAX_SUMMARY_CHARS = 600;

function readOnly(pi: any, name: string): boolean {
	if (READ_ONLY_TOOLS.has(name)) return true;
	const tools: any[] = pi.getAllTools?.() ?? [];
	const hints = tools.find((tool) => tool?.name === name)?.annotations;
	return hints?.readOnlyHint === true && hints?.destructiveHint !== true;
}

// summary is what the human approves: the command for bash, the path for a
// file tool, and the arguments otherwise.
function summary(input: any): string {
	let text: string;
	if (typeof input?.command === "string") text = input.command;
	else if (typeof input?.path === "string") text = input.path;
	else {
		try {
			text = JSON.stringify(input ?? {});
		} catch {
			text = "(arguments not shown)";
		}
	}
	return text.length > MAX_SUMMARY_CHARS ? text.slice(0, MAX_SUMMARY_CHARS) + "…" : text;
}

export default function tclaudePiPermissions(pi: any): void {
	pi.on("tool_call", async (event: any, ctx: any) => {
		const name = typeof event?.toolName === "string" ? event.toolName : "";
		if (name !== "" && readOnly(pi, name)) return;
		const label = name || "an unnamed tool";
		if (!ctx?.hasUI) {
			return { block: true, reason: `tclaude's ask posture: ${label} needs a human's approval, and no human can answer in this mode` };
		}
		const approved = await ctx.ui.confirm(`Allow ${label}?`, summary(event?.input));
		if (approved !== true) {
			return { block: true, reason: `tclaude's ask posture: the human did not approve ${label}` };
		}
	});
}
