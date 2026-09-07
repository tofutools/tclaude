package model

// PresentationPreferences are operator UI choices; they confer no agent authority.
type PresentationPreferences struct {
	NeutralTerminals bool `json:",omitempty"`
	Mode             string
	SoundEnabled     bool
	RadioInRegular   bool
	Channel          string
	MusicVolume      float64
	EffectsVolume    float64
	Revision         Revision
}
type RadioChannel struct{ ID, Name, Description, Group, StreamURL, HomeURL string }

func RadioChannels() []RadioChannel {
	channels := []RadioChannel{
		{ID: "illstreet", Name: "Illinois Street Lounge", Description: "Vintage cocktail and exotica", Group: "vegas"},
		{ID: "secretagent", Name: "Secret Agent", Description: "Spy jazz and surf", Group: "vegas"},
		{ID: "groovesalad", Name: "Groove Salad", Description: "Ambient and downtempo", Group: "vegas"},
		{ID: "lush", Name: "Lush", Description: "Mostly vocal, mostly chilled", Group: "vegas"},
		{ID: "bootliquor", Name: "Boot Liquor", Description: "Americana roots", Group: "vegas"},
		{ID: "u80s", Name: "Underground 80s", Description: "Alternative and new wave", Group: "vegas"},
		{ID: "defcon", Name: "DEF CON Radio", Description: "Music for hacking", Group: "vegas"},
		{ID: "thistle", Name: "The Tavern", Description: "ThistleRadio — Celtic roots", Group: "wizard"},
		{ID: "folkfwd", Name: "The Bard’s Rest", Description: "Folk Forward — indie and folk", Group: "wizard"},
		{ID: "dronezone", Name: "The Astral Plane", Description: "Drone Zone — atmospheric ambient", Group: "wizard"},
		{ID: "darkzone", Name: "The Dungeon", Description: "The Dark Zone — dark ambient", Group: "wizard"},
		{ID: "doomed", Name: "The Crypt", Description: "Doomed — dark industrial ambient", Group: "wizard"},
		{ID: "deepspaceone", Name: "The Cosmos", Description: "Deep Space One — deep-space ambient", Group: "wizard"},
	}
	for i := range channels {
		channels[i].StreamURL = "https://ice1.somafm.com/" + channels[i].ID + "-128-mp3"
		channels[i].HomeURL = "https://somafm.com/" + channels[i].ID + "/"
	}
	return channels
}
func DefaultPresentation() PresentationPreferences {
	return PresentationPreferences{Mode: "regular", MusicVolume: 0.35, EffectsVolume: 0.25}
}
