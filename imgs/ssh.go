package imgs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/picosh/pico/db"
	"github.com/picosh/pico/db/postgres"
	"github.com/picosh/pico/shared"
	"github.com/picosh/pico/shared/storage"
	"github.com/picosh/tunkit"
)

type ctxUserKey struct{}

func getUserCtx(ctx ssh.Context) (*db.User, error) {
	user, ok := ctx.Value(ctxUserKey{}).(*db.User)
	if user == nil || !ok {
		return user, fmt.Errorf("user not set on `ssh.Context()` for connection")
	}
	return user, nil
}
func setUserCtx(ctx ssh.Context, user *db.User) {
	ctx.SetValue(ctxUserKey{}, user)
}

func AuthHandler(dbh db.DB, log *slog.Logger) func(ssh.Context, ssh.PublicKey) bool {
	return func(ctx ssh.Context, key ssh.PublicKey) bool {
		kk, err := shared.KeyForKeyText(key)
		if err != nil {
			log.Error("cannot get pubkey", "err", err)
			return false
		}

		user, err := dbh.FindUserForKey("", kk)
		if err != nil {
			log.Error("user not found", "err", err)
			return false
		}

		if user == nil {
			log.Error("user not found", "err", err)
			return false
		}

		setUserCtx(ctx, user)

		if !dbh.HasFeatureForUser(user.ID, "plus") {
			log.Error("not a pico+ user", "user", user.Name)
			return false
		}

		return true
	}
}

type ErrorHandler struct {
	Err error
}

func (e *ErrorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println(e.Err.Error())
	http.Error(w, e.Err.Error(), http.StatusInternalServerError)
}

func createServeMux(handler *CliHandler, pubsub *tunkit.PubSubHandler) func(ctx ssh.Context) http.Handler {
	return func(ctx ssh.Context) http.Handler {
		router := http.NewServeMux()

		slug := ""
		user, err := getUserCtx(ctx)
		if err == nil && user != nil {
			slug = user.Name
		}

		pubkeys, err := handler.DBPool.FindKeysForUser(user)
		if err != nil {
			handler.Logger.Error("cant get pubkeys for user", "err", err)
			return router
		}

		proxy := httputil.NewSingleHostReverseProxy(&url.URL{
			Scheme: "http",
			Host:   handler.RegistryUrl,
		})

		oldDirector := proxy.Director

		proxy.Director = func(r *http.Request) {
			handler.Logger.Info("director", "request", r)
			oldDirector(r)

			if strings.HasSuffix(r.URL.Path, "_catalog") || r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
				return
			}

			fullPath := strings.TrimPrefix(r.URL.Path, "/v2")

			newPath, err := url.JoinPath("/v2", slug, fullPath)
			if err != nil {
				return
			}

			r.URL.Path = newPath

			query := r.URL.Query()

			if query.Has("from") {
				joinedFrom, err := url.JoinPath(slug, query.Get("from"))
				if err != nil {
					return
				}
				query.Set("from", joinedFrom)

				r.URL.RawQuery = query.Encode()
			}
		}

		proxy.ModifyResponse = func(r *http.Response) error {
			handler.Logger.Info("modify", "request", r)
			shared.CorsHeaders(r.Header)

			if r.Request.Method == http.MethodGet && strings.HasSuffix(r.Request.URL.Path, "_catalog") {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					return err
				}

				err = r.Body.Close()
				if err != nil {
					return err
				}

				var data map[string]any
				err = json.Unmarshal(b, &data)
				if err != nil {
					return err
				}

				newRepos := []string{}

				if repos, ok := data["repositories"].([]any); ok {
					for _, repo := range repos {
						if repoStr, ok := repo.(string); ok && strings.HasPrefix(repoStr, slug) {
							newRepos = append(newRepos, strings.Replace(repoStr, fmt.Sprintf("%s/", slug), "", 1))
						}
					}
				}

				data["repositories"] = newRepos

				newB, err := json.Marshal(data)
				if err != nil {
					return err
				}

				jsonBuf := bytes.NewBuffer(newB)

				r.ContentLength = int64(jsonBuf.Len())
				r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
				r.Body = io.NopCloser(jsonBuf)
			}

			if r.Request.Method == http.MethodGet && (strings.Contains(r.Request.URL.Path, "/tags/") || strings.Contains(r.Request.URL.Path, "/manifests/")) {
				splitPath := strings.Split(r.Request.URL.Path, "/")

				if len(splitPath) > 1 {
					ele := splitPath[len(splitPath)-2]
					if ele == "tags" || ele == "manifests" {
						b, err := io.ReadAll(r.Body)
						if err != nil {
							return err
						}

						err = r.Body.Close()
						if err != nil {
							return err
						}

						var data map[string]any
						err = json.Unmarshal(b, &data)
						if err != nil {
							return err
						}

						if name, ok := data["name"].(string); ok {
							if strings.HasPrefix(name, slug) {
								data["name"] = strings.Replace(name, fmt.Sprintf("%s/", slug), "", 1)
							}
						}

						newB, err := json.Marshal(data)
						if err != nil {
							return err
						}

						jsonBuf := bytes.NewBuffer(newB)

						r.ContentLength = int64(jsonBuf.Len())
						r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
						r.Body = io.NopCloser(jsonBuf)
					}
				}
			}

			if r.Request.Method == http.MethodPut && strings.Contains(r.Request.URL.Path, "/manifests/") {
				digest := r.Header.Get("Docker-Content-Digest")
				forwards := pubsub.GetForwards()
				for _, rf := range forwards {
					ck, err := shared.KeyForKeyText(rf.Pubkey)
					if err != nil {
						continue
					}
					found := false
					for _, pk := range pubkeys {
						if pk.Key == ck {
							found = true
						}
					}
					// event corresponds to a different user, skip
					if !found {
						continue
					}

					// [ ]/v2/erock/alpine/manifests/latest
					splitPath := strings.Split(r.Request.URL.Path, "/")
					img := splitPath[3]
					tag := splitPath[5]

					addr := rf.Listener.Addr()
					furl := fmt.Sprintf(
						"http://%s?digest=%s&image=%s&tag=%s",
						addr.String(),
						url.QueryEscape(digest),
						img,
						tag,
					)
					handler.Logger.Info("sending event", "url", furl)
					_, err = http.Get(furl)
					if err != nil {
						handler.Logger.Error("could not make request to pubsub", "err", err)
					}
				}
			}

			locationHeader := r.Header.Get("location")
			if strings.Contains(locationHeader, fmt.Sprintf("/v2/%s", slug)) {
				r.Header.Set("location", strings.ReplaceAll(locationHeader, fmt.Sprintf("/v2/%s", slug), "/v2"))
			}

			return nil
		}

		router.HandleFunc("/", proxy.ServeHTTP)

		return router
	}
}

func StartSshServer() {
	host := os.Getenv("SSH_HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	port := os.Getenv("SSH_PORT")
	if port == "" {
		port = "2222"
	}
	dbUrl := os.Getenv("DATABASE_URL")
	registryUrl := shared.GetEnv("REGISTRY_URL", "0.0.0.0:5000")
	minioUrl := shared.GetEnv("MINIO_URL", "http://0.0.0.0:9000")
	minioUser := shared.GetEnv("MINIO_ROOT_USER", "")
	minioPass := shared.GetEnv("MINIO_ROOT_PASSWORD", "")

	logger := shared.CreateLogger(false)
	logger.Info("bootup", "registry", registryUrl, "minio", minioUrl)
	dbh := postgres.NewDB(dbUrl, logger)
	st, err := storage.NewStorageMinio(minioUrl, minioUser, minioPass)
	if err != nil {
		panic(err)
	}

	handler := &CliHandler{
		Logger:      logger,
		DBPool:      dbh,
		Storage:     st,
		RegistryUrl: registryUrl,
	}

	pubsub := tunkit.NewPubSubHandler(logger)
	s, err := wish.NewServer(
		wish.WithAddress(fmt.Sprintf("%s:%s", host, port)),
		wish.WithHostKeyPath("ssh_data/term_info_ed25519"),
		wish.WithPublicKeyAuth(AuthHandler(dbh, logger)),
		wish.WithMiddleware(WishMiddleware(handler)),
		tunkit.WithWebTunnel(
			tunkit.NewWebTunnelHandler(createServeMux(handler, pubsub), logger),
		),
		tunkit.WithPubSub(pubsub),
	)

	if err != nil {
		logger.Error("could not create server", "err", err)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	logger.Info("starting SSH server", "host", host, "port", port)
	go func() {
		if err = s.ListenAndServe(); err != nil {
			logger.Error("serve error", "err", err)
			os.Exit(1)
		}
	}()

	<-done
	logger.Info("stopping SSH server")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	if err := s.Shutdown(ctx); err != nil {
		logger.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
