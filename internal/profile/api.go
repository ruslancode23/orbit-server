package profile

import "github.com/ruslancode23/orbit-server/internal/api"

// APIProfile is the one wire shape of a person. Messages, member lists,
// conversation participants and the profile endpoints all go through it, so the
// same account cannot look different depending on which list it came from.
func APIProfile(p Public) api.UserProfile {
	out := api.UserProfile{
		Id:          p.ID,
		Nickname:    p.Nickname,
		DisplayName: p.DisplayName,
		AccentColor: api.AccentColor(p.AccentColor),
		Bio:         p.Bio,
		IsGuest:     p.IsGuest,
		CreatedAt:   p.CreatedAt,
	}
	if p.AvatarURL != "" {
		url := p.AvatarURL
		out.AvatarUrl = &url
	}
	if p.CustomStatus != nil {
		status := api.CustomStatus{ExpiresAt: p.CustomStatus.ExpiresAt}
		if p.CustomStatus.Emoji != "" {
			emoji := p.CustomStatus.Emoji
			status.Emoji = &emoji
		}
		if p.CustomStatus.Text != "" {
			text := p.CustomStatus.Text
			status.Text = &text
		}
		out.CustomStatus = &status
	}
	return out
}
