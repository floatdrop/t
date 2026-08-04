import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// No @wailsio/runtime plugin: this frontend uses no Wails bindings or
// events. All backend communication goes over the loopback WebSocket the
// Go side advertises at /__bridge, so there is nothing to generate.
export default defineConfig({
  server: {
    host: '127.0.0.1',
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: [svelte()],
});
