package pgs

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/picosh/pico/db"
	"github.com/picosh/pico/db/stub"
	"github.com/picosh/pico/shared"
	"github.com/picosh/pico/shared/storage"
	sst "github.com/picosh/pobj/storage"
)

var testUserID = "user-1"
var testUsername = "user"

type ApiExample struct {
	name        string
	path        string
	reqHeaders  map[string]string
	want        string
	wantUrl     string
	status      int
	contentType string

	dbpool  db.DB
	storage map[string]map[string]string
}

type PgsDb struct {
	*stub.StubDB
}

func NewPgsDb(logger *slog.Logger) *PgsDb {
	sb := stub.NewStubDB(logger)
	return &PgsDb{
		StubDB: sb,
	}
}

func (p *PgsDb) FindUserForName(name string) (*db.User, error) {
	return &db.User{
		ID:   testUserID,
		Name: testUsername,
	}, nil
}

func (p *PgsDb) FindProjectByName(userID, name string) (*db.Project, error) {
	return &db.Project{
		ID:         "project-1",
		UserID:     userID,
		Name:       name,
		ProjectDir: name,
		Username:   testUsername,
		Acl: db.ProjectAcl{
			Type: "public",
		},
	}, nil
}

type PgsAnalyticsDb struct {
	*PgsDb
}

func NewPgsAnalticsDb(logger *slog.Logger) *PgsAnalyticsDb {
	return &PgsAnalyticsDb{
		PgsDb: NewPgsDb(logger),
	}
}

func (p *PgsAnalyticsDb) HasFeatureForUser(userID, feature string) bool {
	return true
}

func mkpath(path string) string {
	return fmt.Sprintf("https://%s-test.pgs.test%s", testUsername, path)
}

func TestApiBasic(t *testing.T) {
	bucketName := shared.GetAssetBucketName(testUserID)
	cfg := NewConfigSite()
	cfg.Domain = "pgs.test"
	tt := []*ApiExample{
		{
			name:        "basic",
			path:        "/",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/index.html": "hello world!",
				},
			},
		},
		{
			name:        "direct-file",
			path:        "/test.html",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
		{
			name:        "subdir-301-redirect",
			path:        "/subdir",
			want:        `<a href="/subdir/">Moved Permanently</a>.`,
			status:      http.StatusMovedPermanently,
			contentType: "text/html; charset=utf-8",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/subdir/index.html": "hello world!",
				},
			},
		},
		{
			name:        "redirects-file-301",
			path:        "/anything",
			want:        `<a href="/about.html">Moved Permanently</a>.`,
			status:      http.StatusMovedPermanently,
			contentType: "text/html; charset=utf-8",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/_redirects": "/anything /about.html 301",
					"test/about.html": "hello world!",
				},
			},
		},
		{
			name:        "subdir-direct",
			path:        "/subdir/index.html",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/subdir/index.html": "hello world!",
				},
			},
		},
		{
			name:        "spa",
			path:        "/anything",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/_redirects": "/* /index.html 200",
					"test/index.html": "hello world!",
				},
			},
		},
		{
			name:        "not-found",
			path:        "/anything",
			want:        "404 not found",
			status:      http.StatusNotFound,
			contentType: "text/plain; charset=utf-8",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {},
			},
		},
		{
			name:        "not-found-custom",
			path:        "/anything",
			want:        "boom!",
			status:      http.StatusNotFound,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/404.html": "boom!",
				},
			},
		},
		{
			name:        "images",
			path:        "/profile.jpg",
			want:        "image",
			status:      http.StatusOK,
			contentType: "image/jpeg",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/profile.jpg": "image",
				},
			},
		},
		{
			name:        "redirects-query-param",
			path:        "/anything?query=param",
			want:        `<a href="/about.html?query=param">Moved Permanently</a>.`,
			wantUrl:     "/about.html?query=param",
			status:      http.StatusMovedPermanently,
			contentType: "text/html; charset=utf-8",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/_redirects": "/anything /about.html 301",
					"test/about.html": "hello world!",
				},
			},
		},
		{
			name: "conditional-if-modified-since-future",
			path: "/test.html",
			reqHeaders: map[string]string{
				"If-Modified-Since": time.Now().UTC().Add(time.Hour).Format(http.TimeFormat),
			},
			want:        "",
			status:      http.StatusNotModified,
			contentType: "",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
		{
			name: "conditional-if-modified-since-past",
			path: "/test.html",
			reqHeaders: map[string]string{
				"If-Modified-Since": time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat),
			},
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
		{
			name: "conditional-if-none-match-pass",
			path: "/test.html",
			reqHeaders: map[string]string{
				"If-None-Match": "\"static-etag-for-testing-purposes\"",
			},
			want:        "",
			status:      http.StatusNotModified,
			contentType: "",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
		{
			name: "conditional-if-none-match-fail",
			path: "/test.html",
			reqHeaders: map[string]string{
				"If-None-Match": "\"non-matching-etag\"",
			},
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "text/html",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
		{
			name: "conditional-if-none-match-and-if-modified-since",
			path: "/test.html",
			reqHeaders: map[string]string{
				// The matching etag should take precedence over the past mod time
				"If-None-Match":     "\"static-etag-for-testing-purposes\"",
				"If-Modified-Since": time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat),
			},
			want:        "",
			status:      http.StatusNotModified,
			contentType: "",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/test.html": "hello world!",
				},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", mkpath(tc.path), strings.NewReader(""))
			for key, val := range tc.reqHeaders {
				request.Header.Set(key, val)
			}
			responseRecorder := httptest.NewRecorder()

			st, _ := storage.NewStorageMemory(tc.storage)
			router := NewWebRouter(cfg, cfg.Logger, tc.dbpool, st)
			router.ServeHTTP(responseRecorder, request)

			if responseRecorder.Code != tc.status {
				t.Errorf("Want status '%d', got '%d'", tc.status, responseRecorder.Code)
			}

			ct := responseRecorder.Header().Get("content-type")
			if ct != tc.contentType {
				t.Errorf("Want status '%s', got '%s'", tc.contentType, ct)
			}

			body := strings.TrimSpace(responseRecorder.Body.String())
			if body != tc.want {
				t.Errorf("Want '%s', got '%s'", tc.want, body)
			}

			if tc.wantUrl != "" {
				location, err := responseRecorder.Result().Location()
				if err != nil {
					t.Errorf("err: %s", err.Error())
				}
				if tc.wantUrl != location.String() {
					t.Errorf("Want '%s', got '%s'", tc.wantUrl, location.String())
				}
			}
		})
	}
}

type ImageStorageMemory struct {
	*storage.StorageMemory
	Opts  *storage.ImgProcessOpts
	Fpath string
}

func (s *ImageStorageMemory) ServeObject(bucket sst.Bucket, fpath string, opts *storage.ImgProcessOpts) (io.ReadCloser, *sst.ObjectInfo, error) {
	s.Opts = opts
	s.Fpath = fpath
	info := sst.ObjectInfo{
		Metadata: make(http.Header),
	}
	info.Metadata.Set("content-type", "image/jpeg")
	return io.NopCloser(strings.NewReader("hello world!")), &info, nil
}

func TestImageManipulation(t *testing.T) {
	bucketName := shared.GetAssetBucketName(testUserID)
	cfg := NewConfigSite()
	cfg.Domain = "pgs.test"

	tt := []ApiExample{
		{
			name:        "root-img",
			path:        "/app.jpg/s:500/rt:90",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "image/jpeg",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/app.jpg": "hello world!",
				},
			},
		},
		{
			name:        "root-subdir-img",
			path:        "/subdir/app.jpg/rt:90/s:500",
			want:        "hello world!",
			status:      http.StatusOK,
			contentType: "image/jpeg",

			dbpool: NewPgsDb(cfg.Logger),
			storage: map[string]map[string]string{
				bucketName: {
					"test/subdir/app.jpg": "hello world!",
				},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", mkpath(tc.path), strings.NewReader(""))
			responseRecorder := httptest.NewRecorder()

			memst, _ := storage.NewStorageMemory(tc.storage)
			st := &ImageStorageMemory{
				StorageMemory: memst,
				Opts: &storage.ImgProcessOpts{
					Ratio: &storage.Ratio{},
				},
			}
			router := NewWebRouter(cfg, cfg.Logger, tc.dbpool, st)
			router.ServeHTTP(responseRecorder, request)

			if responseRecorder.Code != tc.status {
				t.Errorf("Want status '%d', got '%d'", tc.status, responseRecorder.Code)
			}

			ct := responseRecorder.Header().Get("content-type")
			if ct != tc.contentType {
				t.Errorf("Want content type '%s', got '%s'", tc.contentType, ct)
			}

			body := strings.TrimSpace(responseRecorder.Body.String())
			if body != tc.want {
				t.Errorf("Want '%s', got '%s'", tc.want, body)
			}

			if st.Opts.Ratio.Width != 500 {
				t.Errorf("Want ratio width '500', got '%d'", st.Opts.Ratio.Width)
				return
			}

			if st.Opts.Rotate != 90 {
				t.Errorf("Want rotate '90', got '%d'", st.Opts.Rotate)
				return
			}
		})
	}
}
