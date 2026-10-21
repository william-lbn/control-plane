import { WebSocketServer } from 'ws';
import { invocationContext } from './context.mjs';

const requests = new WeakMap();
const upgrades = new WeakMap();
export const maxWebSocketBytes = 1024 * 1024;

class ServerSocket extends EventTarget {
  #socket; #state = 0; #protocol = ''; #binaryType = 'arraybuffer';
  #context = invocationContext.getStore();
  CONNECTING = 0; OPEN = 1; CLOSING = 2; CLOSED = 3;
  onopen = null; onmessage = null; onerror = null; onclose = null;
  get readyState() { return this.#socket?.readyState ?? this.#state; }
  get protocol() { return this.#protocol; }
  get extensions() { return ''; }
  get bufferedAmount() { return this.#socket?.bufferedAmount ?? 0; }
  get binaryType() { return this.#binaryType; }
  set binaryType(value) { if (value === 'arraybuffer' || value === 'blob') this.#binaryType = value; }
  #emit(event) { invocationContext.run(this.#context, () => { this.dispatchEvent(event); const fn = this[`on${event.type}`]; if (typeof fn === 'function') fn.call(this, event); }); }
  send(data) {
    if (this.readyState === 0) throw new DOMException('WebSocket is connecting', 'InvalidStateError');
    if (this.readyState !== 1) return;
    const size = typeof data === 'string' ? Buffer.byteLength(data) : data instanceof Blob ? data.size : data?.byteLength;
    if (!Number.isInteger(size)) data = String(data);
    if ((size ?? Buffer.byteLength(data)) > maxWebSocketBytes || this.bufferedAmount > maxWebSocketBytes) { this.#socket.close(1009, 'Message or buffer limit'); return; }
    this.#socket.send(data);
  }
  close(code = 1000, reason = '') {
    if (code !== 1000 && (!Number.isInteger(code) || code < 3000 || code > 4999)) throw new DOMException('Invalid close code', 'InvalidAccessError');
    if (Buffer.byteLength(reason) > 123) throw new DOMException('Close reason exceeds 123 bytes', 'SyntaxError');
    if (this.#socket) this.#socket.close(code, reason);
    else { this.#state = 2; }
  }
  bind(socket, protocol) {
    this.#socket = socket; this.#protocol = protocol; socket.binaryType = 'arraybuffer';
    socket.on('error', () => this.#emit(new Event('error')));
    socket.on('message', (data, binary) => {
      const value = binary ? this.#binaryType === 'blob' ? new Blob([data]) : data : data.toString();
      this.#emit(new MessageEvent('message', { data: value }));
    });
    socket.on('close', (code, reason) => {
      const event = new Event('close');
      Object.defineProperties(event, { code: { value: code }, reason: { value: reason.toString() }, wasClean: { value: code !== 1006 } });
      this.#emit(event);
    });
    if (this.#state === 2) socket.close(1000);
    else this.#emit(new Event('open'));
  }
}

class UpgradeResponse extends Response {
  constructor() { super(null); }
  get status() { return 101; }
  get ok() { return false; }
}

// Self-hosted helper, imported from /opt/neon/functions/runtime/websocket.mjs.
// The response is a capability for this exact request, not a forgeable status.
export function upgradeWebSocket(request, options = {}) {
  const binding = requests.get(request), state = invocationContext.getStore();
  if (!binding || !state || state.closed || binding.upgrade || request.method !== 'GET' || request.headers.get('upgrade')?.toLowerCase() !== 'websocket') throw new TypeError('An active WebSocket handshake is required');
  const protocol = options.protocol ?? '';
  const offered = request.headers.get('sec-websocket-protocol')?.split(',').map(p => p.trim()) ?? [];
  if (typeof protocol !== 'string' || (protocol && !offered.includes(protocol))) throw new TypeError('Select an offered WebSocket protocol');
  const socket = new ServerSocket(), response = new UpgradeResponse();
  const upgrade = { socket, response, protocol, request };
  binding.upgrade = upgrade; upgrades.set(response, upgrade);
  return { socket, response };
}

export function registerWebSocketRequest(request) { const binding = { upgrade: null }; requests.set(request, binding); return binding; }
export function upgradeForResponse(response, request) { const value = upgrades.get(response); return value?.request === request ? value : null; }
export function createWebSocketServer() {
  return new WebSocketServer({ noServer: true, maxPayload: maxWebSocketBytes, perMessageDeflate: false,
    handleProtocols: (_offered, incoming) => incoming.neonSelectedProtocol || false });
}
