type State = { id: string; state: string };
const states = ['queued', 'running', 'retry_wait', 'succeeded', 'failed', 'cancelled'];
const terminal = new Set(['succeeded', 'failed', 'cancelled']);

export function retryAfterSeconds(value: string | null, now = Date.now()): number | undefined {
  if (!value) return undefined;
  const seconds = /^\d+$/.test(value.trim()) ? Number(value) : (Date.parse(value) - now) / 1000;
  return Number.isFinite(seconds) ? Math.max(0, Math.min(600, Math.ceil(seconds))) : undefined;
}

// Only read an already accepted Operation. Never replay the creating mutation
// or claim a backend operation failed because its observation was unavailable.
export function transientOperationRead(error: unknown): boolean {
  if (error instanceof TypeError) return true; // browser fetch network failure
  if (error instanceof DOMException && error.name === 'TimeoutError') return true;
  const status = (error as { status?: unknown } | null)?.status;
  return typeof status === 'number' && [429, 500, 502, 503, 504].includes(status);
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const stop = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', stop);
      resolve();
    }, ms);
    signal.addEventListener('abort', stop, { once: true });
  });
}

export async function followOperation<T extends State>(options: {
  id: string;
  read: (signal: AbortSignal) => Promise<T>;
  signal: AbortSignal;
  onValue: (value: T) => void;
  onUnavailable?: () => void;
  timeoutMs?: number;
  requestTimeoutMs?: number;
  now?: () => number;
  wait?: (ms: number, signal: AbortSignal) => Promise<void>;
}): Promise<T | null> {
  const now = options.now || Date.now;
  const wait = options.wait || delay;
  const deadline = now() + (options.timeoutMs ?? 480_000);
  let failures = 0;
  while (now() < deadline) {
    options.signal.throwIfAborted();
    let value: T;
    try {
      const request = AbortSignal.any([
        options.signal,
        AbortSignal.timeout(
          Math.max(1, Math.min(options.requestTimeoutMs ?? 10_000, deadline - now())),
        ),
      ]);
      value = await options.read(request);
    } catch (error) {
      options.signal.throwIfAborted();
      if (!transientOperationRead(error)) throw error;
      options.onUnavailable?.();
      failures++;
      const hinted = (error as { retryAfterSeconds?: unknown } | null)?.retryAfterSeconds;
      const retryAfter =
        typeof hinted === 'number' && Number.isFinite(hinted)
          ? Math.max(0, Math.min(600, hinted)) * 1000
          : 0;
      await wait(
        Math.min(
          Math.max(retryAfter, Math.min(8_000, 2_000 * 2 ** Math.min(failures - 1, 2))),
          Math.max(0, deadline - now()),
        ),
        options.signal,
      );
      continue;
    }
    options.signal.throwIfAborted();
    if (value.id !== options.id || !states.includes(value.state))
      throw new Error('Invalid Operation response');
    options.onValue(value);
    if (terminal.has(value.state)) return value;
    failures = 0;
    await wait(Math.min(2_000, Math.max(0, deadline - now())), options.signal);
  }
  return null; // observation deadline, not backend failure/cancellation
}
