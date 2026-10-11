package shell

// Coordinated Charm baseline versions approved and pinned for Gostalgia's
// experience layer (Issue #21). Bubbles was blank-imported here only to hold
// the dependency fence; since issue #185 the kit adopts bubbles/viewport for
// bounded scroll windows and keeps the remaining bespoke components for the
// reasons documented at each site. Core runtime, SDK, apps, and CLI tools
// must remain standard-library-only.
const (
	CharmBubbleTeaVersion = "v2.0.10"
	CharmLipGlossVersion  = "v2.0.6"
	CharmBubblesVersion   = "v2.2.1"
)
