<script lang="ts">
  import TerminalView from '$components/TerminalView.svelte';
  import type { Agent } from '$lib/types';

  // Mirrors App.svelte: the view lives under {#if activeAgent}, so dropping
  // the agent tears the view down and hands pending work a null prop.
  let { active }: { active: Agent | null } = $props();
</script>

{#if active}
  {#key active.pane_id}
    <TerminalView
      agent={active}
      allAgents={[active]}
      frame={{ paneId: active.pane_id, content: 'ready', format: 'plain' }}
      responding={new Set<string>()}
    />
  {/key}
{/if}
