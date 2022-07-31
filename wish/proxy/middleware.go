package proxy

import (
	"github.com/charmbracelet/wish"
	"github.com/gliderlabs/ssh"
)

type Router func(sh ssh.Handler, s ssh.Session) []wish.Middleware

func withMiddleware(mdw ...wish.Middleware) ssh.Handler {
	handler := func(s ssh.Session) {}
	for _, mw := range mdw {
		handler = mw(handler)
	}
	return handler
}

func WithProxy(router Router) ssh.Option {
	mdw := func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			mw := router(sh, s)
			fn := withMiddleware(mw...)
			fn(s)
		}
	}

	return wish.WithMiddleware(mdw)
}