const MAX_BLOCKS = 32;
const MAX_SCRIPT_BYTES = 64 * 1024;
const MAX_NAME_BYTES = 128;
const MAX_EXPORTS = 64;

const BLOCK_NAME = /^[A-Za-z0-9][A-Za-z0-9_.-]*$/;
const EXPORT_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
const utf8 = new TextEncoder();

export function sandboxPreLaunchExportNames(value) {
  const text = Array.isArray(value) ? value.join(' ') : String(value || '');
  return text.split(/[\s,]+/u).map((name) => name.trim()).filter(Boolean);
}

export function sandboxPreLaunchEditorRows(blocks) {
  if (!Array.isArray(blocks)) return [];
  return blocks.map((block) => ({
    ...block,
    exports: Array.isArray(block?.exports) ? [...block.exports] : [],
    _exports_text: block?._exports_text
      ?? (Array.isArray(block?.exports) ? block.exports.join(', ') : ''),
  }));
}

export function sandboxPreLaunchForWire(blocks) {
  if (!Array.isArray(blocks)) return [];
  return blocks.map((block) => {
    const exports = sandboxPreLaunchExportNames(
      block?._exports_text ?? (Array.isArray(block?.exports) ? block.exports : []),
    );
    return {
      name: block?.name ?? '',
      script: block?.script ?? '',
      ...(exports.length ? { exports } : {}),
    };
  });
}

export function sandboxPreLaunchValidation(blocks) {
  const profile = [];
  if (!Array.isArray(blocks)) {
    return {
      profile: ['Pre-launch scripts must be an array of blocks.'],
      blocks: [],
      errors: ['Pre-launch scripts must be an array of blocks.'],
    };
  }
  if (blocks.length > MAX_BLOCKS) {
    profile.push(`Pre-launch scripts allow at most ${MAX_BLOCKS} blocks.`);
  }

  const names = new Map();
  for (const block of blocks) {
    if (typeof block?.name !== 'string') continue;
    const name = block.name.trim();
    if (!name) continue;
    names.set(name, (names.get(name) || 0) + 1);
  }

  const perBlock = blocks.map((block) => {
    const errors = { name: [], script: [], exports: [] };
    const name = typeof block?.name === 'string' ? block.name.trim() : '';
    if (!name) {
      errors.name.push('Name is required.');
    } else {
      if (utf8.encode(name).length > MAX_NAME_BYTES) {
        errors.name.push(`Name must be at most ${MAX_NAME_BYTES} bytes.`);
      }
      if (!BLOCK_NAME.test(name)) {
        errors.name.push('Use an alphanumeric first character, then letters, numbers, _, . or -.');
      }
      if ((names.get(name) || 0) > 1) errors.name.push('Block names must be unique.');
    }

    if (typeof block?.script !== 'string' || !block.script.trim()) {
      errors.script.push('Script is required.');
    } else {
      const bytes = utf8.encode(block.script).length;
      if (bytes > MAX_SCRIPT_BYTES) {
        errors.script.push(`Script is ${bytes} bytes; maximum is ${MAX_SCRIPT_BYTES}.`);
      }
      if (block.script.includes('\0')) errors.script.push('Script must not contain a NUL byte.');
    }

    if (block?.exports != null && !Array.isArray(block.exports)) {
      errors.exports.push('Exports must be a comma- or space-separated list.');
    } else {
      const exports = block?.exports || [];
      if (exports.length > MAX_EXPORTS) {
        errors.exports.push(`A block may declare at most ${MAX_EXPORTS} exports.`);
      }
      for (const value of exports) {
        const exportName = typeof value === 'string' ? value.trim() : '';
        if (!exportName || utf8.encode(exportName).length > MAX_NAME_BYTES
          || !EXPORT_NAME.test(exportName)) {
          errors.exports.push(`${JSON.stringify(value)} is not a valid environment-variable name.`);
        }
      }
    }
    return errors;
  });

  const errors = [
    ...profile,
    ...perBlock.flatMap((block) => [...block.name, ...block.script, ...block.exports]),
  ];
  return { profile, blocks: perBlock, errors };
}

