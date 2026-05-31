// A saved workflow using double-quoted strings, trailing commas, and comments —
// all legal JS-literal syntax the static meta parser must tolerate (no JS engine).
export const meta = {
  name: "dq-sample",
  description: "Double quotes, trailing commas, and a /* block */ comment in the literal",
  phases: [
    { title: "Alpha", detail: "first phase", }, /* trailing comma + block comment */
    { title: "Beta", detail: "second phase", },
  ],
}

phase("Alpha")
await agent("step one")
phase("Beta")
await agent("step two")
