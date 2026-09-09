package httpui

import (
	"net/http"
	"strings"
)

// The theme is the one thing a viewer sets. The stylesheet follows the
// operating system's light or dark setting on its own, and a viewer who wants
// the other one says so with the form in the navigation bar. The choice is
// kept in a cookie the browser holds, so the console still writes nothing on
// either side it reads, and it travels back on every request as the data-theme
// attribute of the page.
const (
	themeRoute  = "/theme"
	themeCookie = "theme"
	themeLight  = "light"
	themeDark   = "dark"
	// themeAuto clears the cookie: the page follows the operating system
	// again, which is what a page without the attribute does.
	themeAuto = "auto"
	// themeCookieAge is a year in seconds. A demo console outlives no laptop,
	// and a choice that expired would fall back to the operating system's.
	themeCookieAge = 365 * 24 * 60 * 60
)

// themeFrom reads the viewer's choice. A cookie that is missing or holds
// anything but the two themes is no choice, and the page follows the operating
// system.
func themeFrom(r *http.Request) string {
	cookie, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch cookie.Value {
	case themeLight, themeDark:
		return cookie.Value
	}
	return ""
}

// theme stores the choice the form posted and sends the viewer back to the
// page the form was on. A choice that is none of the three is answered with
// the error page, the way any unreadable parameter is.
func (h *handlers) theme(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.failFrom(w, r, &paramError{name: "theme", reason: "could not be read: " + err.Error()}, nil)
		return
	}

	cookie := &http.Cookie{
		Name:     themeCookie,
		Path:     "/",
		MaxAge:   themeCookieAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	switch choice := r.PostForm.Get("theme"); choice {
	case themeLight, themeDark:
		cookie.Value = choice
	case themeAuto:
		cookie.MaxAge = -1
	default:
		h.failFrom(w, r, &paramError{name: "theme", reason: "is not auto, light or dark"}, nil)
		return
	}

	http.SetCookie(w, cookie)
	http.Redirect(w, r, backPath(r.PostForm.Get("back")), http.StatusSeeOther)
}

// backPath is where the viewer lands after choosing: the page the form was on,
// as long as that is a path of this console. A value carrying a host, an
// absolute URL or a protocol-relative one, would send the viewer off the
// console, and lands on the front page instead.
func backPath(back string) string {
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, `/\`) {
		return "/"
	}
	return back
}
