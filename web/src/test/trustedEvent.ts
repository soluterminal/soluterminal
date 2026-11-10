// jsdom marks every dispatchEvent() untrusted, as a browser does for script.
// A person's input reaches listeners through jsdom's internal dispatch with
// isTrusted set, so do the same here to stand in for a real click or key.
function impl(wrapper: object, key: string): Record<string, unknown> {
  for (const symbol of Object.getOwnPropertySymbols(wrapper)) {
    const value = (wrapper as Record<symbol, unknown>)[symbol];
    if (value && typeof value === 'object' && key in value) return value as Record<string, unknown>;
  }
  throw new Error('not a jsdom wrapper');
}

export function trusted<E extends Event>(event: E): E {
  impl(event, 'isTrusted').isTrusted = true;
  return event;
}

export function dispatchTrusted(target: EventTarget, event: Event): boolean {
  const eventImpl = impl(trusted(event), 'isTrusted');
  const dispatch = impl(target, '_dispatch')._dispatch as (this: unknown, e: unknown) => boolean;
  return dispatch.call(impl(target, '_dispatch'), eventImpl);
}

// Transient user activation, which jsdom does not model: any recent keypress
// or click in the frame sets it, for a few seconds.
export function setUserActivation(isActive: boolean | null): void {
  if (isActive === null) Reflect.deleteProperty(navigator, 'userActivation');
  else Object.defineProperty(navigator, 'userActivation', { value: { isActive }, configurable: true });
}
