// Build the shared profile/role clone payload without re-emitting response
// timestamps. Profile aliases are unique handles owned by the source and must
// stay behind, otherwise the cloned create request deterministically conflicts.
export function clonePayload(source, name) {
  const payload = { ...source, name };
  delete payload.aliases;
  delete payload.created_at;
  delete payload.updated_at;
  return payload;
}
