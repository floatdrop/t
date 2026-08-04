<script lang="ts">
  import { onMount } from 'svelte';
  import { bridge } from './lib/bridge';
  import { store } from './lib/session.svelte';
  import Conference from './components/Conference.svelte';
  import Welcome from './components/Welcome.svelte';
  import DebugDrawer from './components/debug/DebugDrawer.svelte';

  onMount(() => {
    store.attach();
    void bridge.start();

    // Cmd/Ctrl+D toggles the debug drawer — the panels are the point of
    // this app, so reaching them should not need the mouse.
    const onKey = (ev: KeyboardEvent) => {
      if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'd') {
        ev.preventDefault();
        store.debugOpen = !store.debugOpen;
      }
    };
    window.addEventListener('keydown', onKey);

    return () => {
      window.removeEventListener('keydown', onKey);
      store.detach();
      bridge.close();
    };
  });

  const inCall = $derived(store.session.phase === 'joined');
</script>

<main>
  {#if inCall}
    <Conference />
  {:else}
    <Welcome />
  {/if}
</main>
<DebugDrawer />

<style>
  main {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
</style>
