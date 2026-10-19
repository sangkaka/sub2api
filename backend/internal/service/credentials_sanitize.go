package service

// SanitizeStoredCredentials strips secrets that must never be persisted on the
// account credentials map after conversion to OAuth tokens (Grok Web SSO / password).
// Call from admin create/update/import/apply-oauth paths.
//
// Cookie is stripped for every platform except Adobe and Leonardo. Elsewhere it is
// session-jar residue left over after it was exchanged for OAuth tokens — keeping it
// next to those tokens is pure liability. Adobe and Leonardo are architecturally
// different: they have no refresh_token, so the browser cookie *is* the long-lived
// credential and their token refreshers require it. Stripping it there makes the
// channel unable to authenticate.
//
// Bulk paths pass an empty platform label and therefore still strip cookie. That is
// intentional and harmless: bulk credentials are a JSONB merge delta, so dropping the
// key means "leave the stored cookie alone" rather than clearing it.
func SanitizeStoredCredentials(platform string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	for _, key := range []string{
		"password", "sso_token", "sso", "sso-rw", "clearTextPassword",
	} {
		delete(creds, key)
	}
	if platform != PlatformAdobe && platform != PlatformLeonardo {
		delete(creds, "cookie")
	}
	return creds
}
