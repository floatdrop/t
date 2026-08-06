<script lang="ts">
  /**
   * The tlmst mark.
   *
   * The same drawing as icon.svg, redrawn here against the design tokens
   * instead of hardcoded hex: that file has to stand alone as an app icon
   * wherever the OS renders it, while this one lives inside the page and should
   * follow it. Inline rather than an <img> so it costs no request and can
   * inherit colour.
   */
  interface Props {
    /** Rendered size in px, square. */
    size?: number;
  }

  let { size = 72 }: Props = $props();
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 64 64"
  role="img"
  aria-label="tlmst"
  xmlns="http://www.w3.org/2000/svg"
>
  <rect width="64" height="64" rx="15" fill="var(--bg-raised)" />
  <rect x="0.5" y="0.5" width="63" height="63" rx="14.5" fill="none" stroke="var(--border)" />

  <!-- The "t", drawn as strokes rather than <text>: the mark must not depend on
       a font being present. -->
  <g
    fill="none"
    stroke="var(--accent)"
    stroke-width="5"
    stroke-linecap="round"
    stroke-linejoin="round"
  >
    <path d="M27 17V41q0 6 6 6" />
    <path d="M19 27h16" />
  </g>

  <!-- The period is the live dot, in the same green as the speaking indicator,
       so the mark says "a call is happening" rather than just spelling a
       letter. -->
  <circle cx="42.5" cy="44" r="3.5" fill="var(--ok)" />
</svg>
