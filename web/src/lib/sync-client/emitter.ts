// A tiny typed event emitter; listeners run synchronously in registration order.

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Handler = (...args: any[]) => void;

export class Emitter<Events extends Record<string, Handler>> {
  private readonly handlers = new Map<keyof Events, Set<Handler>>();

  on<K extends keyof Events>(event: K, handler: Events[K]): () => void {
    let set = this.handlers.get(event);
    if (!set) {
      set = new Set();
      this.handlers.set(event, set);
    }
    set.add(handler);
    return () => this.off(event, handler);
  }

  off<K extends keyof Events>(event: K, handler: Events[K]): void {
    this.handlers.get(event)?.delete(handler);
  }

  emit<K extends keyof Events>(event: K, ...args: Parameters<Events[K]>): void {
    const set = this.handlers.get(event);
    if (!set) return;
    for (const h of [...set]) {
      try {
        h(...args);
      } catch (err) {
        // A listener must never break the client; surface it asynchronously.
        setTimeout(() => {
          throw err;
        }, 0);
      }
    }
  }

  clear(): void {
    this.handlers.clear();
  }
}
