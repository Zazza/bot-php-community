package news

import (
	"context"
	"database/sql"
	driver "database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"phpbot/internal/llm"
)

// writeWallpaperTriple создаёт в root/theme части тройки {slug}: {slug}-4k.png,
// {slug}-phone.png и preview/{slug}.jpg (отсутствующие части не создаются).
func writeWallpaperTriple(t *testing.T, root, theme, slug string, fourK, phone, preview bool) {
	t.Helper()
	themeDir := filepath.Join(root, theme)
	if err := os.MkdirAll(themeDir, 0o755); err != nil {
		t.Fatalf("mkdir theme %s: %v", themeDir, err)
	}
	if fourK {
		writeWallpaperFile(t, filepath.Join(themeDir, slug+"-4k.png"))
	}
	if phone {
		writeWallpaperFile(t, filepath.Join(themeDir, slug+"-phone.png"))
	}
	if preview {
		pvDir := filepath.Join(themeDir, "preview")
		if err := os.MkdirAll(pvDir, 0o755); err != nil {
			t.Fatalf("mkdir preview %s: %v", pvDir, err)
		}
		writeWallpaperFile(t, filepath.Join(pvDir, slug+".jpg"))
	}
}

func writeWallpaperFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func wallpaperKeys(ws []Wallpaper) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Key())
	}
	return out
}

// TestScanWallpapers — пул собирается только из полных троек: корневые файлы и каталог
// preview темой не считаются, неполная тройка skip'ается, коллизия slug между темами
// разрешается первой по алфавиту темой (ReadDir отдаёт записи sorted).
func TestScanWallpapers(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, root string)
		dir      func(root string) string
		wantKeys []string
		wantErr  bool
	}{
		{
			name: "full_two_themes",
			setup: func(t *testing.T, root string) {
				writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
				writeWallpaperTriple(t, root, "beta", "storm", true, true, true)
			},
			wantKeys: []string{"alpha/pest", "beta/storm"},
		},
		{
			name: "missing_phone_skip",
			setup: func(t *testing.T, root string) {
				writeWallpaperTriple(t, root, "alpha", "pest", true, false, true)
			},
		},
		{
			name: "missing_preview_skip",
			setup: func(t *testing.T, root string) {
				writeWallpaperTriple(t, root, "alpha", "pest", true, true, false)
			},
		},
		{
			name: "root_file_and_preview_dir_not_themes",
			setup: func(t *testing.T, root string) {
				writeWallpaperFile(t, filepath.Join(root, "stray-4k.png"))
				writeWallpaperTriple(t, root, "preview", "x", true, true, true)
			},
		},
		{
			name: "slug_collision_first_theme_wins",
			setup: func(t *testing.T, root string) {
				writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
				writeWallpaperTriple(t, root, "beta", "pest", true, true, true)
			},
			wantKeys: []string{"alpha/pest"},
		},
		{
			name:  "empty_dir",
			setup: func(t *testing.T, root string) {},
		},
		{
			name: "theme_unreadable_skip",
			setup: func(t *testing.T, root string) {
				if os.Geteuid() == 0 {
					t.Skip("root читает chmod-000 каталоги, ветка недостижима")
				}
				writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
				themeDir := filepath.Join(root, "alpha")
				if err := os.Chmod(themeDir, 0o000); err != nil {
					t.Fatalf("chmod theme: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(themeDir, 0o755) })
			},
		},
		{
			name:    "missing_dir_error",
			dir:     func(root string) string { return filepath.Join(root, "nope") },
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if c.setup != nil {
				c.setup(t, root)
			}
			dir := root
			if c.dir != nil {
				dir = c.dir(root)
			}
			pool, err := scanWallpapers(dir)
			if c.wantErr {
				if err == nil {
					t.Fatal("scanWallpapers err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("scanWallpapers: %v", err)
			}
			got := wallpaperKeys(pool)
			sort.Strings(got)
			if len(got) != len(c.wantKeys) {
				t.Fatalf("pool keys = %v, want %v", got, c.wantKeys)
			}
			for i := range got {
				if got[i] != c.wantKeys[i] {
					t.Errorf("pool keys = %v, want %v", got, c.wantKeys)
					break
				}
			}
		})
	}

	t.Run("triple_paths", func(t *testing.T) {
		root := t.TempDir()
		writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
		pool, err := scanWallpapers(root)
		if err != nil {
			t.Fatalf("scanWallpapers: %v", err)
		}
		if len(pool) != 1 {
			t.Fatalf("pool len = %d, want 1", len(pool))
		}
		themeDir := filepath.Join(root, "alpha")
		want := Wallpaper{
			Theme:       "alpha",
			Slug:        "pest",
			Path4K:      filepath.Join(themeDir, "pest-4k.png"),
			PathPhone:   filepath.Join(themeDir, "pest-phone.png"),
			PathPreview: filepath.Join(themeDir, "preview", "pest.jpg"),
		}
		if pool[0] != want {
			t.Errorf("scanned wallpaper = %+v, want %+v", pool[0], want)
		}
	})
}

func pickPool(n int) []Wallpaper {
	pool := make([]Wallpaper, 0, n)
	for i := 0; i < n; i++ {
		pool = append(pool, Wallpaper{Theme: "alpha", Slug: fmt.Sprintf("w%d", i)})
	}
	return pool
}

// TestPickWallpapers — выборка без показанных, сброс цикла при исчерпании, усечение
// до n и полное возвращение пула меньше n. Seed фиксирован (42) для воспроизводимости.
func TestPickWallpapers(t *testing.T) {
	cases := []struct {
		name      string
		pool      int
		shown     []string
		n         int
		wantReset bool
		wantLen   int
	}{
		{"shown_empty_n_unique", 6, nil, 3, false, 3},
		{"partial_shown_excluded", 6, []string{"alpha/w0", "alpha/w1"}, 3, false, 3},
		{"partial_shown_all_fresh", 6, []string{"alpha/w0", "alpha/w1"}, 10, false, 4},
		{"all_shown_resets_cycle", 6, []string{"alpha/w0", "alpha/w1", "alpha/w2", "alpha/w3", "alpha/w4", "alpha/w5"}, 3, true, 3},
		{"pool_smaller_than_n", 2, nil, 3, false, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := pickPool(c.pool)
			shown := make(map[string]struct{}, len(c.shown))
			for _, k := range c.shown {
				shown[k] = struct{}{}
			}
			picks, reset := pickWallpapers(pool, shown, c.n, rand.New(rand.NewSource(42)))
			if reset != c.wantReset {
				t.Errorf("resetCycle = %v, want %v", reset, c.wantReset)
			}
			if len(picks) != c.wantLen {
				t.Fatalf("picks len = %d, want %d", len(picks), c.wantLen)
			}
			poolKeys := make(map[string]struct{}, len(pool))
			for _, k := range wallpaperKeys(pool) {
				poolKeys[k] = struct{}{}
			}
			seen := make(map[string]struct{}, len(picks))
			for _, p := range picks {
				k := p.Key()
				if _, dup := seen[k]; dup {
					t.Errorf("duplicate pick %q", k)
				}
				seen[k] = struct{}{}
				if _, ok := poolKeys[k]; !ok {
					t.Errorf("pick %q not from pool", k)
				}
				if !c.wantReset {
					if _, was := shown[k]; was {
						t.Errorf("shown key %q was picked", k)
					}
				}
			}
		})
	}

	t.Run("same_seed_same_result", func(t *testing.T) {
		pool := pickPool(6)
		a, _ := pickWallpapers(pool, nil, 3, rand.New(rand.NewSource(42)))
		b, _ := pickWallpapers(pool, nil, 3, rand.New(rand.NewSource(42)))
		if !reflect.DeepEqual(a, b) {
			t.Errorf("same seed picks differ: %v vs %v", wallpaperKeys(a), wallpaperKeys(b))
		}
	})

	t.Run("pool_order_not_mutated", func(t *testing.T) {
		pool := pickPool(6)
		before := wallpaperKeys(pool)
		pickWallpapers(pool, nil, 6, rand.New(rand.NewSource(42)))
		if after := wallpaperKeys(pool); !reflect.DeepEqual(before, after) {
			t.Errorf("pool order mutated: %v -> %v", before, after)
		}
	})
}

func TestWallpaperDocName(t *testing.T) {
	cases := []struct {
		slug string
		path string
		want string
	}{
		{"pest-test", "/x/pest-test-4k.png", "php-voronezh-pest-test-4k.png"},
		{"pest-test", "/srv/w/pest-test-phone.png", "php-voronezh-pest-test-phone.png"},
		{"storm", "/a/b/storm-phone.jpeg", "php-voronezh-storm-phone.jpeg"}, // суффикс и расширение из пути
	}
	for _, c := range cases {
		if got := wallpaperDocName(c.slug, c.path); got != c.want {
			t.Errorf("wallpaperDocName(%q, %q) = %q, want %q", c.slug, c.path, got, c.want)
		}
	}
}

func TestWallpapersCaption(t *testing.T) {
	cap := wallpapersCaption()
	if cap == "" {
		t.Fatal("wallpapersCaption is empty")
	}
	if !strings.Contains(cap, "4K") || !strings.Contains(cap, "phone") {
		t.Errorf("wallpapersCaption = %q, want mentions of 4K and phone", cap)
	}
}

type fakeAlbum struct {
	chatID  int64
	paths   []string
	caption string
}

type fakeDoc struct {
	chatID   int64
	path     string
	filename string
}

// fakeMedia — MediaPoster, записывающий вызовы; ошибки включаются конфигурацией.
type fakeMedia struct {
	albums        []fakeAlbum
	docs          []fakeDoc
	photosErr     error
	failDocSuffix string
}

func (m *fakeMedia) PostPhotos(_ context.Context, chatID int64, paths []string, caption string) error {
	if m.photosErr != nil {
		return m.photosErr
	}
	m.albums = append(m.albums, fakeAlbum{chatID: chatID, paths: append([]string(nil), paths...), caption: caption})
	return nil
}

func (m *fakeMedia) PostDocument(_ context.Context, chatID int64, path, filename string) error {
	if m.failDocSuffix != "" && strings.HasSuffix(path, m.failDocSuffix) {
		return errors.New("post document failed")
	}
	m.docs = append(m.docs, fakeDoc{chatID: chatID, path: path, filename: filename})
	return nil
}

// wallpapersDriver/wallpapersConn — минимальный driver.Conn поверх database/sql:
// SELECT отдаёт заданные ключи shown (или ошибку чтения), INSERT/DELETE логируются.
type wallpapersDriver struct{ conn *wallpapersConn }

func (d *wallpapersDriver) Open(string) (driver.Conn, error) { return d.conn, nil }

type wallpapersConn struct {
	shown     []string
	queryErr  error
	execErrOn string
	execLog   *[]string
}

func (c *wallpapersConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("wallpapersConn: prepare unsupported")
}
func (c *wallpapersConn) Close() error { return nil }
func (c *wallpapersConn) Begin() (driver.Tx, error) {
	return nil, errors.New("wallpapersConn: tx unsupported")
}

func (c *wallpapersConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	vals := make([]driver.Value, len(c.shown))
	for i, k := range c.shown {
		vals[i] = k
	}
	return &wallpapersRows{vals: vals}, nil
}

func (c *wallpapersConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if c.execErrOn != "" && strings.Contains(query, c.execErrOn) {
		return nil, errors.New("wallpapers exec failed")
	}
	*c.execLog = append(*c.execLog, query)
	return driver.RowsAffected(0), nil
}

type wallpapersRows struct {
	vals []driver.Value
	next int
}

func (r *wallpapersRows) Columns() []string { return []string{"wallpaper"} }
func (r *wallpapersRows) Close() error      { return nil }
func (r *wallpapersRows) Next(dest []driver.Value) error {
	if r.next >= len(r.vals) {
		return io.EOF
	}
	dest[0] = r.vals[r.next]
	r.next++
	return nil
}

// newWallpapersRepo — *Repository на встроенном fake-драйвере: без Postgres, но с
// честными вызовами ShownWallpapers/MarkWallpapersShown/ResetWallpapers из прод-кода.
func newWallpapersRepo(t *testing.T, shown []string, queryErr error, execErrOn string) (*Repository, *[]string) {
	t.Helper()
	execLog := new([]string)
	name := "wallpapers-sql-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	sql.Register(name, &wallpapersDriver{conn: &wallpapersConn{shown: shown, queryErr: queryErr, execErrOn: execErrOn, execLog: execLog}})
	db, err := sqlx.Open(name, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRepository(db), execLog
}

// TestPostWallpapers — оркестратор: скан → дедуп shown → альбом превью → документы →
// маркировка. Fail-safe ветки: битый/пустой пул → тишина без обращений к media/repo;
// альбом упал → документы не шлются и пометки нет (повтор в следующую пятницу);
// ошибка чтения shown → постим как есть; ошибка документа не мешает маркировке.
func TestPostWallpapers(t *testing.T) {
	singleDir := func(t *testing.T) string {
		root := t.TempDir()
		writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
		return root
	}
	pairDir := func(t *testing.T) string {
		root := t.TempDir()
		writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
		writeWallpaperTriple(t, root, "beta", "storm", true, true, true)
		return root
	}
	singleDocs := []string{"php-voronezh-pest-4k.png", "php-voronezh-pest-phone.png"}
	pairDocs := []string{
		"php-voronezh-pest-4k.png",
		"php-voronezh-pest-phone.png",
		"php-voronezh-storm-4k.png",
		"php-voronezh-storm-phone.png",
	}

	cases := []struct {
		name          string
		build         func(t *testing.T) string
		shown         []string
		shownErr      bool
		photosErr     bool
		failDocSuffix string
		execErrOn     string
		wantAlbums    int
		wantPreviews  int
		wantDocs      []string
		wantInsert    bool
		wantReset     bool
	}{
		{
			name:  "dir_missing_zero_calls",
			build: func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") },
		},
		{
			name:  "dir_empty_zero_calls",
			build: func(t *testing.T) string { return "" },
		},
		{
			name: "pool_empty_zero_calls",
			build: func(t *testing.T) string {
				root := t.TempDir()
				writeWallpaperTriple(t, root, "alpha", "pest", true, false, true) // без phone — тройка skip
				return root
			},
		},
		{
			name:         "single_album_docs_marked",
			build:        singleDir,
			wantAlbums:   1,
			wantPreviews: 1,
			wantDocs:     singleDocs,
			wantInsert:   true,
		},
		{
			name:      "album_failed_no_docs_no_marking",
			build:     singleDir,
			photosErr: true,
		},
		{
			name:         "shown_read_error_posts_anyway",
			build:        singleDir,
			shownErr:     true,
			wantAlbums:   1,
			wantPreviews: 1,
			wantDocs:     singleDocs,
			wantInsert:   true,
		},
		{
			name:         "all_shown_resets_cycle",
			build:        singleDir,
			shown:        []string{"alpha/pest"},
			wantAlbums:   1,
			wantPreviews: 1,
			wantDocs:     singleDocs,
			wantInsert:   true,
			wantReset:    true,
		},
		{
			name:         "pool_of_two_full_batch",
			build:        pairDir,
			wantAlbums:   1,
			wantPreviews: 2,
			wantDocs:     pairDocs,
			wantInsert:   true,
		},
		{
			name:          "doc_error_continues_and_marks",
			build:         singleDir,
			failDocSuffix: "-4k.png",
			wantAlbums:    1,
			wantPreviews:  1,
			wantDocs:      []string{"php-voronezh-pest-phone.png"},
			wantInsert:    true,
		},
		{
			name:         "mark_error_ignored",
			build:        singleDir,
			execErrOn:    "INSERT INTO news_wallpapers",
			wantAlbums:   1,
			wantPreviews: 1,
			wantDocs:     singleDocs,
		},
		{
			name:         "reset_error_still_posts",
			build:        singleDir,
			shown:        []string{"alpha/pest"},
			execErrOn:    "DELETE FROM news_wallpapers",
			wantAlbums:   1,
			wantPreviews: 1,
			wantDocs:     singleDocs,
			wantInsert:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var qErr error
			if c.shownErr {
				qErr = errors.New("shown read failed")
			}
			repo, execLog := newWallpapersRepo(t, c.shown, qErr, c.execErrOn)
			var pErr error
			if c.photosErr {
				pErr = errors.New("album failed")
			}
			media := &fakeMedia{photosErr: pErr, failDocSuffix: c.failDocSuffix}
			d := &Digester{wallpapersDir: c.build(t), repo: repo, media: media}

			d.postWallpapers(42)

			if len(media.albums) != c.wantAlbums {
				t.Fatalf("albums = %d, want %d", len(media.albums), c.wantAlbums)
			}
			if c.wantAlbums > 0 {
				al := media.albums[0]
				if al.chatID != 42 {
					t.Errorf("album chatID = %d, want 42", al.chatID)
				}
				if len(al.paths) != c.wantPreviews {
					t.Errorf("album previews = %d, want %d", len(al.paths), c.wantPreviews)
				}
				for _, p := range al.paths {
					if !strings.HasSuffix(p, ".jpg") {
						t.Errorf("album path %q is not a preview jpg", p)
					}
				}
				if !strings.Contains(al.caption, "Пятничные обои") {
					t.Errorf("album caption = %q, want wallpapers caption", al.caption)
				}
			}
			gotDocs := make([]string, 0, len(media.docs))
			for _, doc := range media.docs {
				if doc.chatID != 42 {
					t.Errorf("doc chatID = %d, want 42", doc.chatID)
				}
				gotDocs = append(gotDocs, doc.filename)
			}
			sort.Strings(gotDocs)
			if c.wantDocs == nil {
				c.wantDocs = []string{}
			}
			if !reflect.DeepEqual(gotDocs, c.wantDocs) {
				t.Errorf("docs = %v, want %v", gotDocs, c.wantDocs)
			}
			var hasInsert, hasReset bool
			for _, q := range *execLog {
				if strings.Contains(q, "INSERT INTO news_wallpapers") {
					hasInsert = true
				}
				if strings.Contains(q, "DELETE FROM news_wallpapers") {
					hasReset = true
				}
			}
			if hasInsert != c.wantInsert {
				t.Errorf("mark shown insert = %v, want %v (execLog: %v)", hasInsert, c.wantInsert, *execLog)
			}
			if hasReset != c.wantReset {
				t.Errorf("cycle reset delete = %v, want %v (execLog: %v)", hasReset, c.wantReset, *execLog)
			}
		})
	}
}

// fakeNewsPoster — Poster пятничного выпуска: провал PostMessage по флагу.
type fakeNewsPoster struct {
	fail  bool
	calls int
}

func (p *fakeNewsPoster) PostMessage(_ context.Context, _ int64, _ string) error {
	p.calls++
	if p.fail {
		return errors.New("post message failed")
	}
	return nil
}

// newFakeLLM — LLMClient над httptest chat/completions: content="" → HTTP 500 (провал LLM).
func newFakeLLM(t *testing.T, content string) *llm.LLMClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if content == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return llm.NewLLMClient(srv.URL, "test-key", "fake-model", 128, 0.9)
}

// fakeLLMDigestBody — одно-блочный выпуск: md-ссылка на allow-домен fakeHosts, без
// голых URL и МЕТА-маркеров — переживает sanitizeFakeBody.
const fakeLLMDigestBody = "**PHP 9.6 исполняет опкоды нативно** — интерпретатор выпилен [подробности](https://github.com/php/php-src/discussions/42)"

// TestPostFakeWallpapersContract — обои постятся ровно один раз и только при полном
// успехе пятничного выпуска; провал LLM/поста и гейты (dir=""/media=nil) — ноль
// обращений к MediaPoster.
func TestPostFakeWallpapersContract(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		posterFails bool
		dir         bool
		withMedia   bool
		wantErr     bool
		wantPosts   int
		wantAlbums  int
		wantDocs    int
	}{
		{name: "digest_ok_wallpapers_once", content: fakeLLMDigestBody, dir: true, withMedia: true, wantPosts: 1, wantAlbums: 1, wantDocs: 2},
		{name: "digest_post_failed_zero", content: fakeLLMDigestBody, posterFails: true, dir: true, withMedia: true, wantErr: true, wantPosts: 1},
		{name: "llm_failed_zero", dir: true, withMedia: true, wantErr: true},
		{name: "gate_empty_dir_zero", content: fakeLLMDigestBody, withMedia: true, wantPosts: 1},
		{name: "gate_nil_media_no_panic", content: fakeLLMDigestBody, dir: true, wantPosts: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, _ := newWallpapersRepo(t, nil, nil, "")
			poster := &fakeNewsPoster{fail: c.posterFails}
			dir := ""
			if c.dir {
				root := t.TempDir()
				writeWallpaperTriple(t, root, "alpha", "pest", true, true, true)
				dir = root
			}
			d := &Digester{llmFake: newFakeLLM(t, c.content), repo: repo, api: poster, wallpapersDir: dir}
			var media *fakeMedia
			if c.withMedia {
				media = &fakeMedia{}
				d.media = media
			}

			err := d.PostFake(context.Background(), 42)

			if (err != nil) != c.wantErr {
				t.Fatalf("PostFake err = %v, wantErr %v", err, c.wantErr)
			}
			if poster.calls != c.wantPosts {
				t.Errorf("digest PostMessage calls = %d, want %d", poster.calls, c.wantPosts)
			}
			albums, docs := 0, 0
			if media != nil {
				albums, docs = len(media.albums), len(media.docs)
			}
			if albums != c.wantAlbums {
				t.Errorf("wallpaper albums = %d, want %d", albums, c.wantAlbums)
			}
			if docs != c.wantDocs {
				t.Errorf("wallpaper docs = %d, want %d", docs, c.wantDocs)
			}
		})
	}
}
