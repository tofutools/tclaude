package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrawingBoardLiveSignalsCoalesceAndForgetTheirOrigin(t *testing.T) {
	hub := &drawingBoardLiveHub{drawingBoards: map[string]map[*drawingBoardLiveSub]struct{}{}}
	sub := hub.subscribe("plan")
	require.NotNil(t, sub)

	hub.publish("plan", "tab-a")
	hub.publish("plan", "tab-a")
	origin, due := hub.take(sub)
	assert.True(t, due)
	assert.Equal(t, "tab-a", origin, "one editor's edits stay its own echo")

	hub.publish("plan", "tab-a")
	hub.publish("plan", "tab-b")
	origin, due = hub.take(sub)
	assert.True(t, due)
	assert.Empty(t, origin, "another editor's change must not pass as tab-a's echo")

	_, due = hub.take(sub)
	assert.False(t, due)

	hub.publish("other", "")
	_, due = hub.take(sub)
	assert.False(t, due, "a drawing board's signals stay on that drawing board")

	hub.unsubscribe("plan", sub)
	assert.Empty(t, hub.drawingBoards)
	assert.Zero(t, hub.count)
}

func TestDrawingBoardLiveBoundsSubscribers(t *testing.T) {
	hub := &drawingBoardLiveHub{drawingBoards: map[string]map[*drawingBoardLiveSub]struct{}{}}
	for range drawingBoardLiveMaxSubscribers {
		require.NotNil(t, hub.subscribe("plan"))
	}
	assert.Nil(t, hub.subscribe("plan"))
}
