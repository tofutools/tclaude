export const meta = {
  name: 'ccwf-fixture-probe',
  description: 'Tiny throwaway workflow to capture CC workflow run-storage layout as test fixtures (JOH-55 step 0)',
  phases: [
    { title: 'Scout', detail: 'one probe agent returns a word' },
    { title: 'Fan', detail: 'two parallel agents return a word each' },
  ],
}

phase('Scout')
log('scouting')
const a = await agent('Output only the single word: alpha. Do not use any tools. Do not explain.', { label: 'scout:alpha', phase: 'Scout' })

phase('Fan')
log('fanning out')
const bs = await parallel([
  () => agent('Output only the single word: bravo. Do not use any tools. Do not explain.', { label: 'fan:bravo', phase: 'Fan' }),
  () => agent('Output only the single word: charlie. Do not use any tools. Do not explain.', { label: 'fan:charlie', phase: 'Fan' }),
])

return { a, bs }
