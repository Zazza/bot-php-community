package news

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const wallpapersPerFriday = 3             // работ в пятничной подборке
const wallpapersTimeout = 4 * time.Minute // бюджет постинга обоев (альбом + 6 документов)
const wallpaperMaxBytes = 50 << 20        // кап файла тройки: реальный 4K PNG ~5–15MB, больше — битый/подменённый

// wallpaperSlugRe — allowlist имени работы: ascii-нижний-регистр/цифры/дефис. Стопорит
// и пустой slug ("-4k.png"), и мусорные имена (пробелы, верхний регистр, unicode).
var wallpaperSlugRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Wallpaper — одна работа пула «Пятничных обоев»: тройка файлов темы {slug}.
type Wallpaper struct {
	Theme       string
	Slug        string
	Path4K      string
	PathPhone   string
	PathPreview string
}

// Key — ключ дедупа в news_wallpapers: "theme/slug".
func (w Wallpaper) Key() string { return w.Theme + "/" + w.Slug }

// scanWallpapers собирает пул из подкаталогов-тем первого уровня. Тема валидна при
// полной тройке {slug}-4k.png + {slug}-phone.png + preview/{slug}.jpg; неполная,
// oversize-тройка или чужой slug — skip + Warn (пул собирается вручную, тихий пропуск
// скрыл бы битую работу навсегда). Корневые файлы и каталог preview игнорируются.
// Slug уникален глобально: коллизия между темами → остаётся первый по алфавиту
// theme (ReadDir отдаёт записи sorted). Внутри темы — только регулярные файлы:
// DirEntry.Type() без lstat отличает symlink/спецфайл (в ro-маунте контейнера это
// сюрприз, а не работа пула).
func scanWallpapers(dir string) ([]Wallpaper, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pool []Wallpaper
	slugs := make(map[string]string)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "preview" {
			continue
		}
		theme := e.Name()
		themeDir := filepath.Join(dir, theme)
		files, err := os.ReadDir(themeDir)
		if err != nil {
			slog.Warn("wallpapers theme unreadable", "theme", theme, "err", err)
			continue
		}
		regular := make(map[string]os.DirEntry, len(files))
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if f.Type() != 0 {
				slog.Warn("wallpapers skip irregular file", "theme", theme, "name", f.Name(), "type", f.Type().String())
				continue
			}
			regular[f.Name()] = f
		}
		for name, fourK := range regular {
			if !strings.HasSuffix(name, "-4k.png") {
				continue
			}
			slug := strings.TrimSuffix(name, "-4k.png")
			if !wallpaperSlugRe.MatchString(slug) {
				slog.Warn("wallpapers skip bad slug", "theme", theme, "slug", slug)
				continue
			}
			if owner, ok := slugs[slug]; ok {
				slog.Warn("wallpapers slug collision", "slug", slug, "kept_theme", owner, "skipped_theme", theme)
				continue
			}
			phone, ok := regular[slug+"-phone.png"]
			if !ok {
				slog.Warn("wallpapers incomplete triple", "theme", theme, "slug", slug, "missing", slug+"-phone.png")
				continue
			}
			previewPath := filepath.Join(themeDir, "preview", slug+".jpg")
			preview, err := os.Stat(previewPath)
			if err != nil {
				slog.Warn("wallpapers incomplete triple", "theme", theme, "slug", slug, "missing", "preview/"+slug+".jpg")
				continue
			}
			fourKInfo, err := fourK.Info()
			if err != nil {
				slog.Warn("wallpapers stat failed", "theme", theme, "slug", slug, "err", err)
				continue
			}
			phoneInfo, err := phone.Info()
			if err != nil {
				slog.Warn("wallpapers stat failed", "theme", theme, "slug", slug, "err", err)
				continue
			}
			if fourKInfo.Size() > wallpaperMaxBytes || phoneInfo.Size() > wallpaperMaxBytes || preview.Size() > wallpaperMaxBytes {
				slog.Warn("wallpapers oversize triple",
					"theme", theme, "slug", slug,
					"4k", fourKInfo.Size(), "phone", phoneInfo.Size(), "preview", preview.Size())
				continue
			}
			slugs[slug] = theme
			pool = append(pool, Wallpaper{
				Theme:       theme,
				Slug:        slug,
				Path4K:      filepath.Join(themeDir, name),
				PathPhone:   filepath.Join(themeDir, slug+"-phone.png"),
				PathPreview: previewPath,
			})
		}
	}
	return pool, nil
}

// pickWallpapers выбирает n работ из непоказанных (свежий цикл). Все показаны →
// resetCycle=true и выборка из полного пула. Свежих меньше n — все свежие. Шаффл
// по копии: порядок пула (и, значит, повторяемость рандома) не мутируется.
func pickWallpapers(pool []Wallpaper, shown map[string]struct{}, n int, rnd *rand.Rand) (picks []Wallpaper, resetCycle bool) {
	fresh := make([]Wallpaper, 0, len(pool))
	for _, w := range pool {
		if _, ok := shown[w.Key()]; !ok {
			fresh = append(fresh, w)
		}
	}
	src := fresh
	if len(src) == 0 {
		src = pool
		resetCycle = true
	}
	picks = make([]Wallpaper, len(src))
	copy(picks, src)
	rnd.Shuffle(len(picks), func(i, j int) { picks[i], picks[j] = picks[j], picks[i] })
	if len(picks) > n {
		picks = picks[:n]
	}
	return picks, resetCycle
}

// wallpaperDocName — имя файла документа: суффикс и расширение берутся из пути
// ("4k.png"/"phone.png"), а не зашиваются — пул единого именования {slug}-*.
func wallpaperDocName(slug, path string) string {
	return "php-voronezh-" + slug + "-" + strings.TrimPrefix(filepath.Base(path), slug+"-")
}

// wallpapersCaption — подпись альбома превью (без числа работ: при усечённом пуле
// их может быть меньше трёх).
func wallpapersCaption() string {
	return "🖼 Пятничные обои. Полные версии без сжатия — файлами ниже: 4K 3840×2160 для десктопа и phone 1170×2532 для смартфона."
}

// MediaPoster — постинг медиа в чат (реализует tg.PosterImpl).
type MediaPoster interface {
	PostPhotos(ctx context.Context, chatID int64, paths []string, caption string) error
	PostDocument(ctx context.Context, chatID int64, path, filename string) error
}

// postWallpapers — пятничная подборка обоев: альбом превью + по два документа на
// работу. Fail-safe: не возвращает error — рушить пятничный выпуск из-за обоев
// нельзя; собственный ctx (4 мин) — не наследует почти истёкший ctx вызывающего.
func (d *Digester) postWallpapers(chatID int64) {
	// Один слот постинга на процесс: cron-пятница и ручной /news fake, совпавшие
	// по времени, без мьютекса дали бы двойную подборку в чат.
	d.mu.Lock()
	defer d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), wallpapersTimeout)
	defer cancel()

	pool, err := scanWallpapers(d.wallpapersDir)
	if err != nil {
		slog.Warn("wallpapers scan", "dir", d.wallpapersDir, "err", err)
		return
	}
	if len(pool) == 0 {
		slog.Warn("wallpapers pool empty", "dir", d.wallpapersDir)
		return
	}
	shown, err := d.repo.ShownWallpapers(ctx, chatID)
	if err != nil {
		// Без истории дедупа постим как есть: обои — развлечение, а не гарантия.
		slog.Warn("wallpapers shown read", "err", err)
		shown = map[string]struct{}{}
	}
	picks, resetCycle := pickWallpapers(pool, shown, wallpapersPerFriday, rand.New(rand.NewSource(time.Now().UnixNano())))
	if len(picks) == 0 {
		return
	}
	previews := make([]string, len(picks))
	for i, w := range picks {
		previews[i] = w.PathPreview
	}
	if err := d.media.PostPhotos(ctx, chatID, previews, wallpapersCaption()); err != nil {
		// Альбом не вышел — не помечаем и не стираем историю: в следующую пятницу
		// попытка повторится на тех же кандидатах.
		slog.Warn("wallpapers post album", "chat_id", chatID, "err", err)
		return
	}
	// Reset только после успешного альбома, но до документов: стёртая при упавшем
	// посте история потеряла бы дедуп пула навсегда.
	if resetCycle {
		slog.Info("wallpapers cycle reset", "chat_id", chatID, "pool", len(pool))
		if err := d.repo.ResetWallpapers(ctx, chatID); err != nil {
			slog.Warn("wallpapers cycle reset", "err", err)
		}
	}
	keys := make([]string, 0, len(picks))
	for _, w := range picks {
		for _, p := range []struct{ path, kind string }{{w.Path4K, "4k"}, {w.PathPhone, "phone"}} {
			if err := d.media.PostDocument(ctx, chatID, p.path, wallpaperDocName(w.Slug, p.path)); err != nil {
				slog.Warn("wallpapers post document", "chat_id", chatID, "slug", w.Slug, "kind", p.kind, "err", err)
			}
		}
		// Осознанно: неудача одного документа не отменяет пометку работы —
		// повторная отсылка шести файлов ради одного хуже молчащей пары.
		keys = append(keys, w.Key())
	}
	// Пометка под свежим ctx (зеркально SaveFake): к концу постинга документов ctx
	// обоев мог почти истечь — отмена пометки = повтор этих же работ в пятницу.
	ctxMark, cancelMark := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelMark()
	if err := d.repo.MarkWallpapersShown(ctxMark, chatID, keys); err != nil {
		slog.Warn("wallpapers mark shown", "err", err)
	}
}
