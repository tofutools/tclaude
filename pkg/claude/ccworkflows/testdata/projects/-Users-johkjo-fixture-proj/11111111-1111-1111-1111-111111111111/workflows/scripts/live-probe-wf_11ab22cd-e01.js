export const meta = {
  name: 'live-probe',
  description: 'Synthetic in-flight run fixture (one agent done, one still running)',
  phases: [
    { title: 'Build', detail: 'two builders in parallel' },
  ],
}

phase('Build')
const xs = await parallel([
  () => agent('Build component A', { label: 'build:a', phase: 'Build' }),
  () => agent('Build component B', { label: 'build:b', phase: 'Build' }),
])

return { xs }
