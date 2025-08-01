package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/target/goalert/config"
	"github.com/target/goalert/expflag"
	"github.com/target/goalert/genericapi"
	"github.com/target/goalert/grafana"
	"github.com/target/goalert/mailgun"
	"github.com/target/goalert/notification/twilio"
	"github.com/target/goalert/permission"
	prometheus "github.com/target/goalert/prometheusalertmanager"
	"github.com/target/goalert/site24x7"
	"github.com/target/goalert/util/errutil"
	"github.com/target/goalert/util/log"
	"github.com/target/goalert/web"
)

func (app *App) initHTTP(ctx context.Context) error {
	middleware := []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(app.Context(req.Context())))
			})
		},

		withSecureHeaders(app.cfg.EnableSecureHeaders, strings.HasPrefix(app.cfg.PublicURL, "https://")),

		config.ShortURLMiddleware,

		// redirect http to https if public URL is https
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				fwdProto := req.Header.Get("x-forwarded-proto")
				if fwdProto != "" {
					req.URL.Scheme = fwdProto
				} else if req.URL.Scheme == "" {
					if req.TLS == nil {
						req.URL.Scheme = "http"
					} else {
						req.URL.Scheme = "https"
					}
				}

				req.URL.Host = req.Host
				cfg := config.FromContext(req.Context())

				if app.cfg.DisableHTTPSRedirect || cfg.ValidReferer(req.URL.String(), req.URL.String()) {
					next.ServeHTTP(w, req)
					return
				}

				u, err := url.ParseRequestURI(req.RequestURI)
				if errutil.HTTPError(req.Context(), w, err) {
					return
				}
				u.Scheme = "https"
				u.Host = req.Host
				if cfg.ValidReferer(req.URL.String(), u.String()) {
					http.Redirect(w, req, u.String(), http.StatusTemporaryRedirect)
					return
				}

				next.ServeHTTP(w, req)
			})
		},

		// limit external calls (fail-safe for loops or DB access)
		extCallLimit(100),

		// request logging
		logRequest(app.cfg.LogRequests),

		// max request time
		timeout(2 * time.Minute),

		func(next http.Handler) http.Handler {
			return http.StripPrefix(app.cfg.HTTPPrefix, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}

				next.ServeHTTP(w, req)
			}))
		},

		// limit max request size
		maxBodySizeMiddleware(app.cfg.MaxReqBodyBytes),

		// authenticate requests
		app.AuthHandler.WrapHandler,

		// add auth info to request logs
		logRequestAuth,

		LimitConcurrencyByAuthSource,

		wrapGzip,
	}

	if app.cfg.Verbose {
		middleware = append(middleware, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(log.WithDebug(req.Context())))
			})
		})
	}

	mux := http.NewServeMux()

	generic := genericapi.NewHandler(genericapi.Config{
		AlertStore:          app.AlertStore,
		IntegrationKeyStore: app.IntegrationKeyStore,
		HeartbeatStore:      app.HeartbeatStore,
		UserStore:           app.UserStore,
	})

	mux.Handle("POST /api/graphql", app.graphql2.Handler())

	mux.HandleFunc("GET /api/v2/config", app.ConfigStore.ServeConfig)
	mux.HandleFunc("PUT /api/v2/config", app.ConfigStore.ServeConfig)

	mux.HandleFunc("GET /api/v2/identity/providers", app.AuthHandler.ServeProviders)
	mux.HandleFunc("POST /api/v2/identity/logout", app.AuthHandler.ServeLogout)

	basicAuth := app.AuthHandler.IdentityProviderHandler("basic")
	mux.HandleFunc("POST /api/v2/identity/providers/basic", basicAuth)

	githubAuth := app.AuthHandler.IdentityProviderHandler("github")
	mux.HandleFunc("POST /api/v2/identity/providers/github", githubAuth)
	mux.HandleFunc("GET /api/v2/identity/providers/github/callback", githubAuth)

	oidcAuth := app.AuthHandler.IdentityProviderHandler("oidc")
	mux.HandleFunc("POST /api/v2/identity/providers/oidc", oidcAuth)
	mux.HandleFunc("GET /api/v2/identity/providers/oidc/callback", oidcAuth)

	if expflag.ContextHas(ctx, expflag.UnivKeys) {
		mux.HandleFunc("POST /api/v2/uik", app.UIKHandler.ServeHTTP)
	}
	mux.HandleFunc("POST /api/v2/mailgun/incoming", mailgun.IngressWebhooks(app.AlertStore, app.IntegrationKeyStore))
	mux.HandleFunc("POST /api/v2/grafana/incoming", grafana.GrafanaToEventsAPI(app.AlertStore, app.IntegrationKeyStore))
	mux.HandleFunc("POST /api/v2/site24x7/incoming", site24x7.Site24x7ToEventsAPI(app.AlertStore, app.IntegrationKeyStore))
	mux.HandleFunc("POST /api/v2/prometheusalertmanager/incoming", prometheus.PrometheusAlertmanagerEventsAPI(app.AlertStore, app.IntegrationKeyStore))

	mux.HandleFunc("POST /api/v2/generic/incoming", generic.ServeCreateAlert)
	mux.HandleFunc("POST /api/v2/heartbeat/{heartbeatID}", generic.ServeHeartbeatCheck)
	mux.HandleFunc("GET /api/v2/user-avatar/{userID}", generic.ServeUserAvatar)
	mux.HandleFunc("GET /api/v2/calendar", app.CalSubStore.ServeICalData)

	mux.HandleFunc("POST /api/v2/twilio/message", app.twilioSMS.ServeMessage)
	mux.HandleFunc("POST /api/v2/twilio/message/status", app.twilioSMS.ServeStatusCallback)
	mux.HandleFunc("POST /api/v2/twilio/call", app.twilioVoice.ServeCall)
	mux.HandleFunc("POST /api/v2/twilio/call/status", app.twilioVoice.ServeStatusCallback)

	mux.HandleFunc("POST /api/v2/slack/message-action", app.slackChan.ServeMessageAction)

	middleware = append(middleware,
		httpRewrite(app.cfg.HTTPPrefix, "/v1/graphql2", "/api/graphql"),
		httpRedirect(app.cfg.HTTPPrefix, "/v1/graphql2/explore", "/api/graphql/explore"),

		httpRewrite(app.cfg.HTTPPrefix, "/v1/config", "/api/v2/config"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/identity/providers", "/api/v2/identity/providers"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/identity/providers/", "/api/v2/identity/providers/"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/identity/logout", "/api/v2/identity/logout"),

		httpRewrite(app.cfg.HTTPPrefix, "/v1/webhooks/mailgun", "/api/v2/mailgun/incoming"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/webhooks/grafana", "/api/v2/grafana/incoming"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/api/alerts", "/api/v2/generic/incoming"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/api/heartbeat/", "/api/v2/heartbeat/"),
		httpRewriteWith(app.cfg.HTTPPrefix, "/v1/api/users/", func(req *http.Request) *http.Request {
			parts := strings.Split(strings.TrimSuffix(req.URL.Path, "/avatar"), "/")
			req.URL.Path = "/api/v2/user-avatar/" + parts[len(parts)-1]
			return req
		}),

		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/sms/messages", "/api/v2/twilio/message"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/sms/status", "/api/v2/twilio/message/status"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/call", "/api/v2/twilio/call?type=alert"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/alert-status", "/api/v2/twilio/call?type=alert-status"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/test", "/api/v2/twilio/call?type=test"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/stop", "/api/v2/twilio/call?type=stop"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/verify", "/api/v2/twilio/call?type=verify"),
		httpRewrite(app.cfg.HTTPPrefix, "/v1/twilio/voice/status", "/api/v2/twilio/call/status"),

		func(next http.Handler) http.Handler {
			twilioHandler := twilio.WrapValidation(
				// go back to the regular mux after validation
				twilio.WrapHeaderHack(next),
				*app.twilioConfig,
			)
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.HasPrefix(req.URL.Path, "/api/v2/twilio/") {
					twilioHandler.ServeHTTP(w, req)
					return
				}

				next.ServeHTTP(w, req)
			})
		},
	)

	mux.HandleFunc("GET /health", app.healthCheck)
	mux.HandleFunc("GET /health/engine", app.engineStatus)
	mux.HandleFunc("GET /health/engine/cycle", app.engineCycle)
	mux.Handle("GET /health/", http.NotFoundHandler())

	webH, err := web.NewHandler(app.cfg.UIDir, app.cfg.HTTPPrefix)
	if err != nil {
		return err
	}

	// This is necessary so that we can return 404 for invalid/unknown API routes, otherwise it will get caught by the UI handler and incorrectly return the index.html or a 405 (Method Not Allowed) error.
	mux.Handle("GET /api/", http.NotFoundHandler())
	mux.Handle("POST /api/", http.NotFoundHandler())
	mux.Handle("GET /v1/", http.NotFoundHandler())
	mux.Handle("POST /v1/", http.NotFoundHandler())

	// non-API/404s go to UI handler and return index.html
	mux.Handle("GET /", webH)

	mux.Handle("GET /api/graphql/explore", webH)
	mux.Handle("GET /api/graphql/explore/", webH)

	mux.HandleFunc("GET /admin/riverui/", func(w http.ResponseWriter, r *http.Request) {
		err := permission.LimitCheckAny(r.Context(), permission.Admin)
		if permission.IsUnauthorized(err) {
			// render login since we're on a UI route
			webH.ServeHTTP(w, r)
			return
		}
		if errutil.HTTPError(r.Context(), w, err) {
			return
		}

		app.RiverUI.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /admin/riverui/api/", func(w http.ResponseWriter, r *http.Request) {
		err := permission.LimitCheckAny(r.Context(), permission.Admin)
		if errutil.HTTPError(r.Context(), w, err) {
			return
		}

		app.RiverUI.ServeHTTP(w, r)
	})

	app.srv = &http.Server{
		Handler: applyMiddleware(mux, middleware...),

		ReadHeaderTimeout: time.Second * 30,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       time.Minute * 2,
		MaxHeaderBytes:    app.cfg.MaxReqHeaderBytes,
	}
	app.srv.Handler = promhttp.InstrumentHandlerInFlight(metricReqInFlight, app.srv.Handler)
	app.srv.Handler = promhttp.InstrumentHandlerCounter(metricReqTotal, app.srv.Handler)

	// Ingress/load balancer/proxy can do a keep-alive, backend doesn't need it.
	// It also makes zero downtime deploys nearly impossible; an idle connection
	// could have an in-flight request when the server closes it.
	app.srv.SetKeepAlivesEnabled(false)

	return nil
}
