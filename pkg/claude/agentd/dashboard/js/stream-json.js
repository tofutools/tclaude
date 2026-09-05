// Read flushed NDJSON records from a fetch response. Chunk boundaries need not
// match records or UTF-8 characters. Ending mid-record is a protocol error.
async function* readJSONLines(response) {
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = '';
  try {
    while (true) {
      const { value, done } = await reader.read();
      buffered += decoder.decode(value, { stream: !done });
      let newline;
      while ((newline = buffered.indexOf('\n')) >= 0) {
        if (newline > 1024 * 1024) throw new Error('Progress record too large');
        const line = buffered.slice(0, newline); buffered = buffered.slice(newline + 1);
        if (line.trim()) yield JSON.parse(line);
      }
      if (buffered.length > 1024 * 1024) throw new Error('Progress record too large');
      if (done) break;
    }
    if (buffered.trim()) throw new Error('Progress stream ended mid-record');
  } finally {
    try { await reader.cancel(); } catch { /* Connection already closed. */ }
    reader.releaseLock();
  }
}

export { readJSONLines };
