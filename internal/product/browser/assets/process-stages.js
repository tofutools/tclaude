// Ordered-check operations adapted from legacy process-node-form.js at
// 68a5eb9c4442b9aaeb5d845e5213778516a77992. Only the v2 field adapter differs.
export function addCheck(stages, performer) {
 const checks = stages.Checks || [], taken = new Set(checks.map(check => check.ID));
 let id = 'check';
 for (let n = 2; taken.has(id); n += 1) id = `check-${n}`;
 checks.push({ID:id, Name:id, Performer:structuredClone(performer)});
 stages.Checks = checks;
 return id;
}
export function removeCheck(stages, index) {
 const checks = stages.Checks || [];
 if (index < 0 || index >= checks.length) throw new Error(`no check at ${index}`);
 checks.splice(index, 1);
 if (!checks.length) delete stages.Checks;
}
export function moveCheck(stages, index, delta) {
 const checks = stages.Checks || [], target = index + delta;
 if (index < 0 || index >= checks.length) throw new Error(`no check at ${index}`);
 if (target < 0 || target >= checks.length) return;
 const [check] = checks.splice(index, 1); checks.splice(target, 0, check);
}
