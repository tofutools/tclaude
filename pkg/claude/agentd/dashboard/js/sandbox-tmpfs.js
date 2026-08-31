// Editor-side model for the sandbox profile's temporary filesystems (TCL-1218).
//
// The shape is deliberately small — {path, size?} — and this module mirrors
// sandbox-pre-launch.js: stable editor row ids so typing in one row cannot
// remount another, a wire projection that strips editor-only fields, and a
// validation pass whose messages are the ones the daemon would produce, said
// earlier and beside the field.
//
// The daemon remains authoritative. Everything here exists so an operator finds
// out about a bad row while they are looking at it, not after a save round trip.

const MAX_MOUNTS = 32;
const MAX_PATH_BYTES = 4096;

// The same quantity grammar the memory limit accepts, and refused for the same
// reason it is refused server-side: a bare number has no unit, so `1048576`
// would be an ambiguous ceiling rather than a small one.
const SIZE = /^(?:\d+(?:\.\d+)?|\.\d+)(?:[KMGT](?:I(?:B)?|B)?|B)$/i;
const ZERO_SIZE = /^(?:0+(?:\.0*)?|\.0+)[A-Za-z]+$/i;

const utf8 = new TextEncoder();
let editorRowSequence = 0;

function nextEditorRowID() {
  editorRowSequence += 1;
  return `tmpfs-editor-${editorRowSequence}`;
}

export function sandboxTmpfsEditorRows(mounts, previous = []) {
  if (!Array.isArray(mounts)) return [];
  const previousByPath = new Map(previous.flatMap((row) => {
    const path = typeof row?.path === 'string' ? row.path.trim() : '';
    return path && row?._editor_id ? [[path, row._editor_id]] : [];
  }));
  return mounts.map((row, index) => {
    const path = typeof row?.path === 'string' ? row.path.trim() : '';
    return {
      path: row?.path ?? '',
      size: row?.size ?? '',
      _editor_id: row?._editor_id || previousByPath.get(path)
        || previous[index]?._editor_id || nextEditorRowID(),
    };
  });
}

export function sandboxTmpfsNewEditorRow() {
  return { path: '', size: '', _editor_id: nextEditorRowID() };
}

// size_bytes is deliberately never sent: it is derived server-side from the
// authored spelling, and a client that also sent it would be asserting a
// number it did not compute.
export function sandboxTmpfsForWire(mounts) {
  if (!Array.isArray(mounts)) return [];
  return mounts.map((row) => {
    const size = String(row?.size ?? '').trim();
    return { path: String(row?.path ?? '').trim(), ...(size ? { size } : {}) };
  });
}

// filesystem is passed so the one cross-field conflict the daemon refuses — a
// sandbox path claimed by both a tmpfs and a filesystem rule — is reported on
// the row that causes it rather than as an opaque save failure.
export function sandboxTmpfsValidation(mounts, filesystem = []) {
  const profile = [];
  if (!Array.isArray(mounts)) {
    const message = 'Temporary filesystems must be an array of mounts.';
    return { profile: [message], mounts: [], errors: [message] };
  }
  if (mounts.length > MAX_MOUNTS) {
    profile.push(`A profile may mount at most ${MAX_MOUNTS} temporary filesystems.`);
  }

  const counts = new Map();
  for (const row of mounts) {
    const path = String(row?.path ?? '').trim();
    if (!path) continue;
    counts.set(path, (counts.get(path) || 0) + 1);
  }
  // Keyed on the guest path a filesystem row occupies, which is its mount_path
  // when it carries one — that is the position a tmpfs would collide with.
  const claimed = new Map();
  for (const row of Array.isArray(filesystem) ? filesystem : []) {
    const mountPath = String(row?.mount_path ?? '').trim();
    const guest = mountPath || String(row?.path ?? '').trim();
    if (guest) claimed.set(guest, String(row?.access ?? 'read'));
  }

  const perMount = mounts.map((row) => {
    const errors = { path: [], size: [] };
    const path = String(row?.path ?? '').trim();
    if (!path) {
      errors.path.push('Path is required.');
    } else {
      if (!path.startsWith('/') && !path.startsWith('~')) {
        errors.path.push('Path must be absolute (or start with ~ for the daemon’s home).');
      }
      if (path === '/') errors.path.push('The sandbox root cannot be a temporary filesystem.');
      if (utf8.encode(path).length > MAX_PATH_BYTES) {
        errors.path.push(`Path must be at most ${MAX_PATH_BYTES} bytes.`);
      }
      if ((counts.get(path) || 0) > 1) errors.path.push('Each sandbox path may be mounted once.');
      if (claimed.has(path)) {
        errors.path.push(
          `This path is already claimed by a ${claimed.get(path)} filesystem rule; a sandbox path is either a temporary filesystem or a filesystem rule, not both.`,
        );
      }
    }

    const size = String(row?.size ?? '').trim();
    if (size) {
      if (!SIZE.test(size)) {
        errors.size.push('Size must be a quantity with a B, K/KB/KiB, M/MB/MiB, G/GB/GiB, or T/TB/TiB unit, such as 512MiB.');
      } else if (ZERO_SIZE.test(size)) {
        errors.size.push('Size must be greater than zero.');
      }
    }
    return errors;
  });

  const errors = [
    ...profile,
    ...perMount.flatMap((row) => [...row.path, ...row.size]),
  ];
  return { profile, mounts: perMount, errors };
}
