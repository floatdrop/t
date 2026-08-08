import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// No @wailsio/runtime plugin: this frontend uses no Wails bindings or
// events. All backend communication goes over the loopback WebSocket the
// Go side advertises at /__bridge, so there is nothing to generate.
export default defineConfig({
  // Unit tests cover the plain-TypeScript policy modules — the arithmetic that
  // decides what to send and what to ask for. Nothing here renders a
  // component: the parts of this frontend that touch a camera, a codec or a
  // socket are exercised by running the app, and the parts that are pure
  // decisions are exercised here.
  test: {
    include: ['src/**/*.test.ts'],
    environment: 'node',
  },
  server: {
    host: '127.0.0.1',
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: [svelte()],
});
