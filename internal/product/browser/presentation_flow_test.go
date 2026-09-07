package browser

import (
	"github.com/go-rod/rod/lib/proto"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBrowserWizardPreferencesRadioAndReducedMotion(t *testing.T) {
	_, page, _ := processEditorBrowser(t)
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("#presentation-mode").MustSelect("Wizard")
	page.MustElementR("#presentation-status", "saved")
	require.True(t, page.MustEval(`() => document.body.classList.contains('wizard')`).Bool())
	require.Equal(t, "Parties", page.MustElement("[data-tab=groups]").MustText())
	require.Equal(t, "Summon familiar", page.MustElement("#new-agent").MustText())
	require.Contains(t, page.MustInfo().URL, "wizard=1")
	page.MustElement("[data-tab=radio]").MustClick()
	require.Equal(t, "thistle", page.MustElement("#radio-station").MustProperty("value").Str())
	page.MustElement("#radio-station").MustSelect("Astral Plane")
	page.MustElementR("#presentation-status", "saved")
	page.MustEval(`() => { const i=document.querySelector('#music-volume');i.value='.17';i.dispatchEvent(new Event('change')); }`)
	page.MustWait(`() => document.querySelector('#presentation-status').textContent.includes('saved') && !presentation.saving`)
	// Replace only browser media playback and the external station metadata boundary.
	page.MustEval(`() => { window.radioCalls=0;HTMLMediaElement.prototype.play=function(){window.radioCalls++;return Promise.resolve()};HTMLMediaElement.prototype.pause=function(){};const original=presentation.api;presentation.api=(p,...args)=>p.startsWith('/radio/')?Promise.resolve({artist:'Fixture bard',title:'The Tower'}):original(p,...args); }`)
	page.MustElement("#radio-play").MustClick()
	page.MustElementR("#radio-status", "Live")
	page.MustElementR("#radio-track", "Fixture bard")
	page.MustElement("#presentation-sound").MustClick()
	page.MustElementR("#radio-status", "Sound muted")
	page.MustWait(`() => !presentation.saving`)
	require.NoError(t, proto.EmulationSetEmulatedMedia{Features: []*proto.EmulationMediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}}}.Call(page))
	page.MustEval(`() => {document.querySelectorAll('.presentation-spark').forEach(n=>n.remove());presentation.burst(100,100,10)}`)
	require.Zero(t, page.MustEval(`() => document.querySelectorAll('.presentation-spark').length`).Int())
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	require.True(t, page.MustEval(`() => document.body.classList.contains('wizard')`).Bool())
	require.Equal(t, "dronezone", page.MustEval(`() => presentation.prefs.Channel`).Str())
	require.Equal(t, .17, page.MustEval(`() => presentation.prefs.MusicVolume`).Num())
	require.False(t, page.MustEval(`() => presentation.prefs.SoundEnabled`).Bool())
	require.False(t, page.MustEval(`() => presentation.intent`).Bool())
	page.MustElement("#presentation-controls summary").MustClick()
	page.MustElement("#presentation-mode").MustSelect("Regular")
	page.MustWait(`() => !presentation.saving`)
	require.Equal(t, "Groups", page.MustElement("[data-tab=groups]").MustText())
	require.True(t, page.MustEval(`() => document.querySelector('#radio-tab').hidden`).Bool())
	page.MustElement("#radio-regular").MustClick()
	page.MustWait(`() => !presentation.saving`)
	require.False(t, page.MustEval(`() => document.querySelector('#radio-tab').hidden`).Bool())
	require.Equal(t, "dronezone", page.MustEval(`() => presentation.current`).Str())
}
