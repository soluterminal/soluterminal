import { currentRoutingScope, signStreamUrl, streamSigningNeeded, type RoutingScope } from '../api';
import { openPrivateRelayWebSocket, privateRelayTarget } from './privateRelay';

// Streams to a machine this device enrolled with must be signed (approved
// devices, lib/deviceIdentity.ts), and signing is async. This stand-in lets a
// synchronous `new WebSocket(url)` call site keep its shape: it signs first,
// then opens the real socket and forwards its events. Every other machine
// gets the real object directly. (Event streams sign through
// openTicketedEventSource's prepare step.)

type Listener = [string, EventListenerOrEventListenerObject, boolean | AddEventListenerOptions | undefined];

class DeferredWebSocket {
  onopen: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onclose: ((this: WebSocket, ev: CloseEvent) => unknown) | null = null;
  onerror: ((this: WebSocket, ev: Event) => unknown) | null = null;
  onmessage: ((this: WebSocket, ev: MessageEvent) => unknown) | null = null;
  binaryType: BinaryType = 'blob';
  private real: WebSocket | null = null;
  private closed = false;
  private listeners: Listener[] = [];

  constructor(readonly url: string, protocols: string | string[] | undefined, scope?: RoutingScope) {
    void signStreamUrl(url, scope).then((signed) => {
      if (this.closed) return;
      const ws = new WebSocket(signed, protocols);
      ws.binaryType = this.binaryType;
      this.real = ws;
      for (const [type, l, o] of this.listeners) ws.addEventListener(type, l, o);
      const self = this as unknown as WebSocket;
      ws.onopen = (ev) => this.onopen?.call(self, ev);
      ws.onclose = (ev) => this.onclose?.call(self, ev);
      ws.onerror = (ev) => this.onerror?.call(self, ev);
      ws.onmessage = (ev) => this.onmessage?.call(self, ev);
    });
  }

  get readyState(): number {
    if (this.real) return this.real.readyState;
    return this.closed ? WebSocket.CLOSED : WebSocket.CONNECTING;
  }

  get protocol(): string {
    return this.real?.protocol ?? '';
  }

  get bufferedAmount(): number {
    return this.real?.bufferedAmount ?? 0;
  }

  addEventListener(type: string, l: EventListenerOrEventListenerObject, o?: boolean | AddEventListenerOptions): void {
    this.listeners.push([type, l, o]);
    this.real?.addEventListener(type, l, o);
  }

  removeEventListener(type: string, l: EventListenerOrEventListenerObject, o?: boolean | EventListenerOptions): void {
    this.listeners = this.listeners.filter(([t, x]) => t !== type || x !== l);
    this.real?.removeEventListener(type, l, o);
  }

  send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
    // Same contract as a real socket that hasn't opened yet.
    if (!this.real) throw new DOMException('WebSocket is not open', 'InvalidStateError');
    this.real.send(data);
  }

  close(code?: number, reason?: string): void {
    this.closed = true;
    this.real?.close(code, reason);
  }
}

/** Whether sockets for scope go through the private relay channel (lib/privateRelay). */
export function privateRelaySocketsFor(scope?: RoutingScope): boolean {
  const s = scope ?? currentRoutingScope();
  return privateRelayTarget(s.prefix, s.backendPrefix) !== null;
}

/**
 * `new WebSocket(url, protocols)`, signed first when the machine requires it,
 * or carried by the private relay channel when that is turned on for the
 * machine (then only by it: if the channel cannot open the socket fails, and
 * the caller's reconnect loop tries again).
 */
export function openWebSocket(url: string, protocols?: string | string[], scope?: RoutingScope): WebSocket {
  if (privateRelaySocketsFor(scope)) {
    const s = scope ?? currentRoutingScope();
    return openPrivateRelayWebSocket(url, protocols, s);
  }
  if (!streamSigningNeeded(scope, url)) return new WebSocket(url, protocols);
  return new DeferredWebSocket(url, protocols, scope) as unknown as WebSocket;
}
