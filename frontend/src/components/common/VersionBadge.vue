<template>
  <span
    v-if="version"
    class="rounded-lg bg-gray-100 px-2 py-1 text-xs font-medium text-gray-600 dark:bg-dark-800 dark:text-dark-400"
    aria-label="Version"
  >
    v{{ version }}
  </span>
</template>

<script setup lang="ts">
defineProps<{
  version?: string
}>()
</script>
