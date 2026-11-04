export function resolveSiteName(value?: string): string {
  const name = value?.trim()
  return !name || name === 'Sub2API' ? 'SHub' : name
}
