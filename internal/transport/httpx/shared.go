package httpx

import (
	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/storage"
)

// identityFromSession builds the overlay identity from a viewer session, or nil.
func identityFromSession(session *storage.Session) *core.OverlayIdentity {
	if session == nil {
		return nil
	}
	id := &core.OverlayIdentity{Login: session.Login}
	if session.AvatarURL != nil {
		id.AvatarURL = session.AvatarURL
	}
	if session.Name != "" {
		id.Name = session.Name
	}
	return id
}

// authorFromSession builds a comment Author from a viewer session, or nil in
// anonymous (local) mode.
func authorFromSession(session *storage.Session) *core.Author {
	if session == nil {
		return nil
	}
	a := &core.Author{Login: session.Login}
	if session.AvatarURL != nil {
		a.AvatarURL = session.AvatarURL
	}
	if session.Name != "" {
		a.Name = session.Name
	}
	return a
}
