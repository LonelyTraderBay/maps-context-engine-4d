import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string): MediaQueryList => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => undefined,
    removeListener: () => undefined,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
    dispatchEvent: () => false,
  }),
});

class TestResizeObserver implements ResizeObserver {
  private readonly callback: ResizeObserverCallback;

  constructor(callback: ResizeObserverCallback) {
    this.callback = callback;
  }

  observe(target: Element): void {
    this.callback([{
      target,
      contentRect: new DOMRect(0, 0, 800, 600),
      borderBoxSize: [],
      contentBoxSize: [],
      devicePixelContentBoxSize: [],
    }], this);
  }

  unobserve(): void {}
  disconnect(): void {}
}

Object.defineProperty(window, "ResizeObserver", { writable: true, value: TestResizeObserver });
Object.defineProperty(globalThis, "ResizeObserver", { writable: true, value: TestResizeObserver });

afterEach(() => cleanup());
