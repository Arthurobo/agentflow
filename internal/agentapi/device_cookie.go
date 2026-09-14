package agentapi

import (
	"context"
	"net/http"
	"time"
)

// On the public listeners the web app itself is only served to browsers that
// have paired (see httpserve), and a page load can't carry an Authorization
// header. Pairing over a public listener therefore also sets DeviceCookie,
// which holds the device token. The cookie only decides whether the app's
// files are served: the API ignores it and authenticates by the header alone,
// so a cross-site request that carries the cookie gains nothing. That is also
// why it is SameSite=Lax rather than Strict: a paired phone that opens the
// URL from a link in a mail or chat app arrives by a cross-site navigation,
// and must still get the app rather than a blank page.

// DeviceCookie is the name of the cookie that marks a paired browser.
const DeviceCookie = "af_device"

// deviceCookieMaxAge keeps the cookie as long as browsers allow; the device
// token itself lasts until it is revoked.
const deviceCookieMaxAge = 400 * 24 * time.Hour

type publicListenerKey struct{}

// onPublicListener marks requests that arrived on a public listener.
func onPublicListener(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), publicListenerKey{}, true)))
	})
}

func isPublicListener(r *http.Request) bool {
	v, _ := r.Context().Value(publicListenerKey{}).(bool)
	return v
}

// setDeviceCookie sets DeviceCookie to token when r came in on a public
// listener. The local listener serves the app to anyone on this computer
// and needs no cookie.
func setDeviceCookie(w http.ResponseWriter, r *http.Request, token string) {
	if !isPublicListener(r) {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     DeviceCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(deviceCookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// handlePairCookie is POST /pair/cookie: a browser that is already paired
// (it sends its device token in the header) gets the cookie, for devices
// paired before the public listeners required it.
func (s *Server) handlePairCookie(w http.ResponseWriter, r *http.Request) {
	setDeviceCookie(w, r, bearer(r.Header.Get("Authorization")))
	w.WriteHeader(http.StatusNoContent)
}

// DeviceTokenValid reports whether token belongs to an active paired device.
// A revoked device, a pairing token or an unknown token is not valid.
func (s *Server) DeviceTokenValid(ctx context.Context, token string) bool {
	if token == "" || s.st == nil {
		return false
	}
	d, err := s.st.VerifyClientDevice(ctx, token)
	return err == nil && d != nil
}
