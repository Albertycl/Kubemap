// Pure helpers for the log view (KM-80), kept free of React and the DOM so
// node --test can check them.

/** A followed log arrives as NDJSON: {"data": "<base64>"} lines, then one {"end": {...}}. */
export interface StreamLine {
  data?: string;
  end?: { reason?: string; error?: { code: string; message: string } };
}

/**
 * Splits a byte stream of NDJSON into lines. Network chunks do not follow
 * line boundaries, so an unfinished line is kept for the next push.
 */
export class NdjsonSplitter {
  private text = new TextDecoder();
  private rest = "";

  push(chunk: Uint8Array): StreamLine[] {
    this.rest += this.text.decode(chunk, { stream: true });
    const parts = this.rest.split("\n");
    this.rest = parts.pop() ?? "";
    const out: StreamLine[] = [];
    for (const p of parts) {
      if (p.trim() === "") continue;
      out.push(JSON.parse(p) as StreamLine);
    }
    return out;
  }
}

/** Base64 (as Go encodes []byte in JSON) to bytes. */
export function base64Bytes(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// CSI sequences (colours, cursor moves) and OSC sequences (titles, links).
const ANSI = /\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g;

/** Removes terminal escape codes, which many logs carry for colour. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI, "");
}

/**
 * The text of a log view, capped at max characters: when it grows past the
 * cap, the oldest whole lines are dropped. dropped counts what was removed.
 */
export class LogBuffer {
  text = "";
  dropped = 0;
  private max: number;
  constructor(max = 2_000_000) {
    this.max = max;
  }

  append(s: string): void {
    this.text += s;
    if (this.text.length <= this.max) return;
    let cut = this.text.length - this.max;
    const nl = this.text.indexOf("\n", cut);
    if (nl >= 0) cut = nl + 1;
    this.dropped += cut;
    this.text = this.text.slice(cut);
  }

  clear(): void {
    this.text = "";
    this.dropped = 0;
  }
}

/**
 * Strips escape codes from streamed text. A code can be split across two
 * chunks, so an unfinished one at the end is held back until the next push.
 */
export class AnsiStripper {
  private held = "";

  push(s: string): string {
    s = this.held + s;
    this.held = "";
    const esc = s.lastIndexOf("\x1b");
    if (esc >= 0 && esc >= s.length - 64) {
      const tail = s.slice(esc);
      // Finished if the whole tail is removed as one code, or it is not a code.
      if (stripAnsi(tail) === tail && /^\x1b(?:\[[0-?]*[ -/]*|\][^\x07\x1b]*|\x1b?)$/.test(tail)) {
        this.held = tail;
        s = s.slice(0, esc);
      }
    }
    return stripAnsi(s);
  }
}
