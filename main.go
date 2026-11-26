package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

//go:embed index.html
var indexHTML []byte

var unsafeChars = regexp.MustCompile(`[\\/:*?"<>|\r\n]+`)

type CardConfig struct {
	Ratio       string `json:"ratio"`
	CustomW     int    `json:"customW"`
	CustomH     int    `json:"customH"`
	FontFamily  string `json:"fontFamily"`
	FontSize    int    `json:"fontSize"`
	ThemeIndex  int    `json:"themeIndex"`
	BgColor     string `json:"bgColor"`
	CardColor   string `json:"cardColor"`
	TextColor   string `json:"textColor"`
	AccentColor string `json:"accentColor"`
	ShowDate    bool   `json:"showDate"`
	DatePos     string `json:"datePos"`
	Suffix      string `json:"suffix"`
}

type ScheduleConfig struct {
	Enabled          bool   `json:"enabled"`
	Time             string `json:"time"`             // "HH:MM", e.g. "00:00"
	Count            int    `json:"count"`            // default count per library, e.g. 20
	Suffix           string `json:"suffix"`           // optional folder suffix
	AutoResetOnEmpty bool   `json:"autoResetOnEmpty"` // true: start new round after all tasks exhausted
	LastRunDate      string `json:"lastRunDate"`      // "YYYY-MM-DD"
	LastRunTime      string `json:"lastRunTime"`
	LastRunSummary   string `json:"lastRunSummary"`
}

type TaskLibrary struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Tasks     []string `json:"tasks"`
	Remaining []string `json:"remaining"`
	Round     int      `json:"round"`
	Enabled   bool     `json:"enabled"` // whether included in scheduled batch generation
	Count     int      `json:"count"`   // 0 = use global Schedule.Count
}

type AppState struct {
	Libraries       []TaskLibrary  `json:"libraries"`
	ActiveLibraryID string         `json:"activeLibraryId"`
	NoRepeat        bool           `json:"noRepeat"`
	CurrentTask     string         `json:"currentTask"`
	Card            CardConfig     `json:"card"`
	Schedule        ScheduleConfig `json:"schedule"`

	// Legacy fields for seamless migration from v1 state.json
	LegacyTasks     []string `json:"tasks,omitempty"`
	LegacyRemaining []string `json:"remaining,omitempty"`
	LegacyRound     int      `json:"round,omitempty"`
}

type Store struct {
	mu        sync.Mutex
	state     AppState
	statePath string
	outputDir string
	fontsDir  string
	fontCache *sfnt.Font
	fontName  string
}

func defaultState() AppState {
	tasks := []string{
		"今日自律打卡\n早起喝一杯温水\n记录当下感受",
		"整理手机相册\n删掉 20 张不需要的照片",
		"读完一本书的任意一个章节\n写 1 句话总结",
		"晚饭后出门散步 20 分钟\n全程不看手机",
		"清理电脑桌面和下载文件夹",
	}
	schedEnabled := strings.EqualFold(os.Getenv("SCHEDULE_ENABLED"), "true")
	schedTime := os.Getenv("SCHEDULE_TIME")
	if schedTime == "" {
		schedTime = "00:00"
	}
	schedCount := 20
	if v, err := strconv.Atoi(os.Getenv("SCHEDULE_COUNT")); err == nil && v > 0 {
		schedCount = v
	}
	schedSuffix := strings.TrimSpace(os.Getenv("SCHEDULE_SUFFIX"))

	return AppState{
		Libraries: []TaskLibrary{
			{
				ID:        "lib-1",
				Name:      "1号库",
				Tasks:     tasks,
				Remaining: shuffleStrings(tasks),
				Round:     1,
				Enabled:   true,
				Count:     0,
			},
		},
		ActiveLibraryID: "lib-1",
		NoRepeat:        true,
		Card: CardConfig{
			Ratio:       "1080x1440",
			CustomW:     1080,
			CustomH:     1440,
			FontFamily:  `"PingFang SC", "Microsoft YaHei", "Noto Sans SC", sans-serif`,
			FontSize:    64,
			ThemeIndex:  0,
			BgColor:     "#ffffff",
			CardColor:   "#f8f9fb",
			TextColor:   "#1f2329",
			AccentColor: "#8f959e",
			ShowDate:    true,
			DatePos:     "bottom-right",
			Suffix:      schedSuffix,
		},
		Schedule: ScheduleConfig{
			Enabled:          schedEnabled,
			Time:             schedTime,
			Count:            schedCount,
			Suffix:           schedSuffix,
			AutoResetOnEmpty: true,
		},
	}
}

func cryptoRandInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

func shuffleStrings(src []string) []string {
	out := make([]string, len(src))
	copy(out, src)
	for i := len(out) - 1; i > 0; i-- {
		j := cryptoRandInt(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func normalizeTaskText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func cleanTaskList(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = normalizeTaskText(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func newStore(dataDir, outputDir, fontsDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.MkdirAll(fontsDir, 0o755)

	st := &Store{
		state:     defaultState(),
		statePath: filepath.Join(dataDir, "state.json"),
		outputDir: outputDir,
		fontsDir:  fontsDir,
	}

	if raw, err := os.ReadFile(st.statePath); err == nil {
		var loaded AppState
		if err := json.Unmarshal(raw, &loaded); err == nil {
			// Migrate legacy single-list state to library "1号库"
			if len(loaded.Libraries) == 0 && len(loaded.LegacyTasks) > 0 {
				round := loaded.LegacyRound
				if round <= 0 {
					round = 1
				}
				rem := loaded.LegacyRemaining
				if rem == nil {
					rem = shuffleStrings(loaded.LegacyTasks)
				}
				loaded.Libraries = []TaskLibrary{
					{
						ID:        "lib-1",
						Name:      "1号库",
						Tasks:     cleanTaskList(loaded.LegacyTasks),
						Remaining: rem,
						Round:     round,
						Enabled:   true,
						Count:     0,
					},
				}
				loaded.ActiveLibraryID = "lib-1"
			}
			loaded.LegacyTasks = nil
			loaded.LegacyRemaining = nil
			loaded.LegacyRound = 0

			if len(loaded.Libraries) == 0 {
				loaded.Libraries = defaultState().Libraries
				loaded.ActiveLibraryID = loaded.Libraries[0].ID
			}
			for i := range loaded.Libraries {
				if loaded.Libraries[i].ID == "" {
					loaded.Libraries[i].ID = fmt.Sprintf("lib-%d", i+1)
				}
				if strings.TrimSpace(loaded.Libraries[i].Name) == "" {
					loaded.Libraries[i].Name = fmt.Sprintf("%d号库", i+1)
				}
				if loaded.Libraries[i].Round <= 0 {
					loaded.Libraries[i].Round = 1
				}
				if loaded.Libraries[i].Tasks == nil {
					loaded.Libraries[i].Tasks = []string{}
				}
				if loaded.Libraries[i].Remaining == nil {
					loaded.Libraries[i].Remaining = []string{}
				}
			}
			if loaded.ActiveLibraryID == "" {
				loaded.ActiveLibraryID = loaded.Libraries[0].ID
			}
			if loaded.Schedule.Time == "" {
				loaded.Schedule.Time = "00:00"
			}
			if loaded.Schedule.Count <= 0 {
				loaded.Schedule.Count = 20
			}
			st.state = loaded
			_ = st.saveLocked()
		}
	} else {
		_ = st.saveLocked()
	}

	st.loadBestFont()
	return st, nil
}

func (s *Store) saveLocked() error {
	s.state.LegacyTasks = nil
	s.state.LegacyRemaining = nil
	s.state.LegacyRound = 0
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

func (s *Store) findLibraryLocked(id string) *TaskLibrary {
	for i := range s.state.Libraries {
		if s.state.Libraries[i].ID == id {
			return &s.state.Libraries[i]
		}
	}
	if len(s.state.Libraries) > 0 {
		return &s.state.Libraries[0]
	}
	return nil
}

func (s *Store) loadBestFont() {
	candidates := []string{}
	if entries, err := os.ReadDir(s.fontsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext == ".ttf" || ext == ".otf" || ext == ".ttc" {
				candidates = append(candidates, filepath.Join(s.fontsDir, e.Name()))
			}
		}
	}
	sysDirs := []string{
		"/usr/share/fonts",
		"/usr/local/share/fonts",
		`C:\Windows\Fonts`,
	}
	preferredNames := []string{
		"NotoSansCJK-Regular.ttc",
		"NotoSansSC-Regular.ttf",
		"NotoSansSC-Regular.otf",
		"WenQuanYi",
		"wqy",
		"LXGW",
		"msyh.ttc",
		"simhei.ttf",
		"simsun.ttc",
	}
	for _, dir := range sysDirs {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if ext != ".ttf" && ext != ".otf" && ext != ".ttc" {
				return nil
			}
			for _, pref := range preferredNames {
				if strings.Contains(strings.ToLower(d.Name()), strings.ToLower(pref)) {
					candidates = append(candidates, p)
					break
				}
			}
			return nil
		})
	}

	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f *sfnt.Font
		if strings.ToLower(filepath.Ext(p)) == ".ttc" {
			col, err := opentype.ParseCollection(data)
			if err == nil && col.NumFonts() > 0 {
				f, err = col.Font(0)
			}
		} else {
			f, err = opentype.Parse(data)
		}
		if err == nil && f != nil {
			s.fontCache = f
			s.fontName = filepath.Base(p)
			log.Printf("loaded server font: %s", p)
			return
		}
	}
	log.Printf("warning: no CJK ttf/otf/ttc font found in %s or system fonts", s.fontsDir)
}

func (s *Store) drawOneFromLibLocked(lib *TaskLibrary) (string, bool) {
	if lib == nil || len(lib.Tasks) == 0 {
		return "", false
	}
	if !s.state.NoRepeat {
		task := lib.Tasks[cryptoRandInt(len(lib.Tasks))]
		s.state.CurrentTask = task
		return task, true
	}
	if len(lib.Remaining) == 0 {
		if !s.state.Schedule.AutoResetOnEmpty {
			return "", false
		}
		lib.Remaining = shuffleStrings(lib.Tasks)
		lib.Round++
	}
	task := lib.Remaining[0]
	lib.Remaining = lib.Remaining[1:]
	s.state.CurrentTask = task
	return task, true
}

func (s *Store) drawBatchFromLibLocked(lib *TaskLibrary, count int) ([]string, int) {
	if lib == nil || len(lib.Tasks) == 0 || count <= 0 {
		return nil, 0
	}
	picked := make([]string, 0, count)
	inBatch := make(map[string]struct{}, count)

	for len(picked) < count {
		if !s.state.NoRepeat {
			task := lib.Tasks[cryptoRandInt(len(lib.Tasks))]
			picked = append(picked, task)
			continue
		}
		if len(lib.Remaining) == 0 {
			if !s.state.Schedule.AutoResetOnEmpty {
				break
			}
			lib.Remaining = shuffleStrings(lib.Tasks)
			lib.Round++
		}
		chosenIdx := 0
		if len(lib.Tasks) >= count {
			for i, cand := range lib.Remaining {
				if _, dup := inBatch[cand]; !dup {
					chosenIdx = i
					break
				}
			}
		}
		task := lib.Remaining[chosenIdx]
		lib.Remaining = append(lib.Remaining[:chosenIdx], lib.Remaining[chosenIdx+1:]...)
		picked = append(picked, task)
		inBatch[task] = struct{}{}
	}
	if len(picked) > 0 {
		s.state.CurrentTask = picked[len(picked)-1]
	}
	return picked, len(lib.Remaining)
}

func parseHexColor(hexStr string, fallback color.RGBA) color.RGBA {
	s := strings.TrimPrefix(strings.TrimSpace(hexStr), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return fallback
	}
	r, err1 := strconv.ParseUint(s[0:2], 16, 8)
	g, err2 := strconv.ParseUint(s[2:4], 16, 8)
	b, err3 := strconv.ParseUint(s[4:6], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return fallback
	}
	return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
}

func fillRoundRect(img *image.RGBA, x0, y0, w, h, r int, col color.RGBA) {
	x1, y1 := x0+w, y0+h
	rf := float64(r)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			inside := true
			var cx, cy float64
			if x < x0+r && y < y0+r {
				cx, cy = float64(x0+r), float64(y0+r)
				inside = math.Hypot(float64(x)-cx, float64(y)-cy) <= rf
			} else if x >= x1-r && y < y0+r {
				cx, cy = float64(x1-r-1), float64(y0+r)
				inside = math.Hypot(float64(x)-cx, float64(y)-cy) <= rf
			} else if x < x0+r && y >= y1-r {
				cx, cy = float64(x0+r), float64(y1-r-1)
				inside = math.Hypot(float64(x)-cx, float64(y)-cy) <= rf
			} else if x >= x1-r && y >= y1-r {
				cx, cy = float64(x1-r-1), float64(y1-r-1)
				inside = math.Hypot(float64(x)-cx, float64(y)-cy) <= rf
			}
			if inside {
				img.SetRGBA(x, y, col)
			}
		}
	}
}

func wrapText(face font.Face, text string, maxWidthPx int) []string {
	paragraphs := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var lines []string
	for _, p := range paragraphs {
		if p == "" {
			lines = append(lines, "")
			continue
		}
		var cur strings.Builder
		for _, r := range p {
			test := cur.String() + string(r)
			if font.MeasureString(face, test).Ceil() > maxWidthPx && cur.Len() > 0 {
				lines = append(lines, cur.String())
				cur.Reset()
				cur.WriteRune(r)
			} else {
				cur.WriteRune(r)
			}
		}
		if cur.Len() > 0 {
			lines = append(lines, cur.String())
		}
	}
	return lines
}

func (s *Store) getDims(cfg CardConfig) (int, int) {
	if cfg.Ratio == "custom" {
		w, h := cfg.CustomW, cfg.CustomH
		if w < 200 || w > 4096 {
			w = 1080
		}
		if h < 200 || h > 4096 {
			h = 1440
		}
		return w, h
	}
	parts := strings.Split(cfg.Ratio, "x")
	if len(parts) == 2 {
		w, err1 := strconv.Atoi(parts[0])
		h, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && w >= 200 && h >= 200 {
			return w, h
		}
	}
	return 1080, 1440
}

func (s *Store) renderCardPNG(text, dateStr string, cfg CardConfig) ([]byte, error) {
	w, h := s.getDims(cfg)
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	bgCol := parseHexColor(cfg.BgColor, color.RGBA{255, 255, 255, 255})
	cardCol := parseHexColor(cfg.CardColor, bgCol)
	textCol := parseHexColor(cfg.TextColor, color.RGBA{31, 35, 41, 255})
	accentCol := parseHexColor(cfg.AccentColor, color.RGBA{143, 149, 158, 255})

	draw.Draw(img, img.Bounds(), &image.Uniform{bgCol}, image.Point{}, draw.Src)

	minDim := w
	if h < minDim {
		minDim = h
	}
	pad := int(math.Round(float64(minDim) * 0.065))
	fillRoundRect(img, pad, pad, w-pad*2, h-pad*2, 36, cardCol)

	fontSize := cfg.FontSize
	if fontSize < 24 {
		fontSize = 64
	}

	var mainFace font.Face = basicfont.Face7x13
	var dateFace font.Face = basicfont.Face7x13
	if s.fontCache != nil {
		if f, err := opentype.NewFace(s.fontCache, &opentype.FaceOptions{
			Size:    float64(fontSize),
			DPI:     72,
			Hinting: font.HintingFull,
		}); err == nil {
			mainFace = f
			defer f.Close()
		}
		dateSize := math.Max(24, math.Round(float64(minDim)*0.03))
		if f, err := opentype.NewFace(s.fontCache, &opentype.FaceOptions{
			Size:    dateSize,
			DPI:     72,
			Hinting: font.HintingFull,
		}); err == nil {
			dateFace = f
			defer f.Close()
		}
	}

	maxTextW := w - int(math.Round(float64(w)*0.28))
	lines := wrapText(mainFace, text, maxTextW)
	lineH := int(math.Round(float64(fontSize) * 1.55))
	totalH := len(lines) * lineH
	ascent := mainFace.Metrics().Ascent.Ceil()
	startY := (h-totalH)/2 + ascent + (lineH-fontSize)/2

	drawer := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{textCol},
		Face: mainFace,
	}
	for i, line := range lines {
		lw := font.MeasureString(mainFace, line).Ceil()
		x := (w - lw) / 2
		y := startY + i*lineH
		drawer.Dot = fixed.P(x, y)
		drawer.DrawString(line)
	}

	if cfg.ShowDate && dateStr != "" {
		margin := int(math.Round(float64(minDim) * 0.11))
		dw := font.MeasureString(dateFace, dateStr).Ceil()
		dAscent := dateFace.Metrics().Ascent.Ceil()
		var dx, dy int
		switch cfg.DatePos {
		case "bottom-center":
			dx = (w - dw) / 2
			dy = h - margin
		case "top-left":
			dx = margin
			dy = margin + dAscent
		default:
			dx = w - margin - dw
			dy = h - margin
		}
		dateDrawer := &font.Drawer{
			Dst:  img,
			Src:  &image.Uniform{accentCol},
			Face: dateFace,
			Dot:  fixed.P(dx, dy),
		}
		dateDrawer.DrawString(dateStr)
	}

	return encodePNG(img)
}

func encodePNG(img image.Image) ([]byte, error) {
	var b []byte
	w := &byteWriter{buf: &b}
	if err := png.Encode(w, img); err != nil {
		return nil, err
	}
	return b, nil
}

type byteWriter struct{ buf *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

func sanitizeSegment(s string) string {
	s = strings.TrimSpace(unsafeChars.ReplaceAllString(s, ""))
	s = strings.ReplaceAll(s, "..", "")
	return s
}

func formatDateFolder(dateStr, suffix string) string {
	compact := strings.ReplaceAll(sanitizeSegment(dateStr), "-", "")
	if compact == "" {
		compact = time.Now().Format("20060102")
	}
	cleanSuffix := sanitizeSegment(suffix)
	if cleanSuffix != "" {
		return compact + "_" + cleanSuffix
	}
	return compact
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

type libBatchJob struct {
	libID   string
	libName string
	tasks   []string
	rem     int
	round   int
}

func (s *Store) runBatch(countOverride int, dateStr, suffix, onlyLibID, trigger string) (map[string]any, error) {
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}

	s.mu.Lock()
	if suffix == "" {
		suffix = s.state.Schedule.Suffix
	}
	defaultCount := s.state.Schedule.Count
	if defaultCount <= 0 {
		defaultCount = 20
	}
	cfg := s.state.Card

	jobs := make([]libBatchJob, 0, len(s.state.Libraries))
	for i := range s.state.Libraries {
		lib := &s.state.Libraries[i]
		if onlyLibID != "" {
			if lib.ID != onlyLibID {
				continue
			}
		} else if !lib.Enabled {
			continue
		}
		if len(lib.Tasks) == 0 {
			continue
		}
		cnt := defaultCount
		if lib.Count > 0 {
			cnt = lib.Count
		}
		if countOverride > 0 {
			cnt = countOverride
		}
		picked, rem := s.drawBatchFromLibLocked(lib, cnt)
		if len(picked) > 0 {
			jobs = append(jobs, libBatchJob{
				libID:   lib.ID,
				libName: lib.Name,
				tasks:   picked,
				rem:     rem,
				round:   lib.Round,
			})
		}
	}
	s.mu.Unlock()

	if len(jobs) == 0 {
		return map[string]any{
			"ok":        false,
			"error":     "没有可生成的任务（请检查任务库是否已开启且不为空，或本轮已抽完）",
			"generated": 0,
		}, nil
	}

	dateFolder := formatDateFolder(dateStr, suffix)
	tsPrefix := time.Now().Format("150405")
	totalSaved := 0
	partSummaries := make([]string, 0, len(jobs))
	savedFiles := make([]string, 0)

	for _, job := range jobs {
		libFolder := sanitizeSegment(job.libName)
		if libFolder == "" {
			libFolder = sanitizeSegment(job.libID)
		}
		targetDir := filepath.Join(s.outputDir, dateFolder, libFolder)
		if !strings.HasPrefix(targetDir, s.outputDir) {
			return nil, fmt.Errorf("invalid target dir")
		}
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return nil, err
		}

		for idx, task := range job.tasks {
			pngBytes, err := s.renderCardPNG(task, dateStr, cfg)
			if err != nil {
				return nil, err
			}
			firstLine := strings.Split(task, "\n")[0]
			cleanTitle := truncateRunes(sanitizeSegment(firstLine), 16)
			if cleanTitle == "" {
				cleanTitle = "task"
			}
			fileName := fmt.Sprintf("%02d_%s_%s.png", idx+1, tsPrefix, cleanTitle)
			fullPath := filepath.Join(targetDir, fileName)
			if err := os.WriteFile(fullPath, pngBytes, 0o644); err != nil {
				return nil, err
			}
			savedFiles = append(savedFiles, filepath.ToSlash(filepath.Join(dateFolder, libFolder, fileName)))
			totalSaved++
		}
		partSummaries = append(partSummaries, fmt.Sprintf("%s %d张(剩%d)", libFolder, len(job.tasks), job.rem))
	}

	now := time.Now()
	summary := fmt.Sprintf("[%s] %s/ -> %s", trigger, dateFolder, strings.Join(partSummaries, "、"))

	s.mu.Lock()
	if trigger == "定时" {
		s.state.Schedule.LastRunDate = now.Format("2006-01-02")
	}
	s.state.Schedule.LastRunTime = now.Format("2006-01-02 15:04:05")
	s.state.Schedule.LastRunSummary = summary
	_ = s.saveLocked()
	s.mu.Unlock()

	log.Printf("batch done: %s", summary)
	debug.FreeOSMemory()
	return map[string]any{
		"ok":        true,
		"folder":    dateFolder,
		"generated": totalSaved,
		"files":     savedFiles,
		"summary":   summary,
	}, nil
}

func (s *Store) startScheduler() {
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			today := now.Format("2006-01-02")
			hhmm := now.Format("15:04")

			s.mu.Lock()
			enabled := s.state.Schedule.Enabled
			targetTime := strings.TrimSpace(s.state.Schedule.Time)
			lastRunDate := s.state.Schedule.LastRunDate
			suffix := s.state.Schedule.Suffix
			s.mu.Unlock()

			if !enabled || targetTime == "" || lastRunDate == today {
				continue
			}
			if hhmm == targetTime {
				if _, err := s.runBatch(0, today, suffix, "", "定时"); err != nil {
					log.Printf("scheduled batch failed: %v", err)
				}
			}
		}
	}()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func updateLibraryTasks(lib *TaskLibrary, incoming []string) {
	newTasks := cleanTaskList(incoming)
	oldSet := make(map[string]struct{}, len(lib.Tasks))
	for _, t := range lib.Tasks {
		oldSet[t] = struct{}{}
	}
	newSet := make(map[string]struct{}, len(newTasks))
	for _, t := range newTasks {
		newSet[t] = struct{}{}
	}
	keptRem := make([]string, 0, len(newTasks))
	for _, t := range lib.Remaining {
		if _, exists := newSet[t]; exists {
			keptRem = append(keptRem, t)
		}
	}
	added := make([]string, 0)
	for _, t := range newTasks {
		if _, existed := oldSet[t]; !existed {
			added = append(added, t)
		}
	}
	if len(added) > 0 {
		keptRem = shuffleStrings(append(keptRem, added...))
	}
	lib.Tasks = newTasks
	lib.Remaining = keptRem
}

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	outputDir := os.Getenv("OUTPUT_DIR")
	if outputDir == "" {
		outputDir = "./output"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	fontsDir := os.Getenv("FONTS_DIR")
	if fontsDir == "" {
		fontsDir = "./fonts"
	}

	absOutput, err := filepath.Abs(outputDir)
	if err != nil {
		log.Fatalf("invalid OUTPUT_DIR: %v", err)
	}
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		log.Fatalf("invalid DATA_DIR: %v", err)
	}
	absFonts, err := filepath.Abs(fontsDir)
	if err != nil {
		log.Fatalf("invalid FONTS_DIR: %v", err)
	}

	store, err := newStore(absData, absOutput, absFonts)
	if err != nil {
		log.Fatalf("init store failed: %v", err)
	}
	store.startScheduler()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"mode":       "server",
			"outputDir":  outputDir,
			"fontLoaded": store.fontName,
			"serverTime": time.Now().Format("2006-01-02 15:04:05"),
		})
	})

	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			store.mu.Lock()
			st := store.state
			store.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st})

		case http.MethodPost:
			var req struct {
				ActiveLibraryID *string `json:"activeLibraryId"`
				NoRepeat        *bool   `json:"noRepeat"`
				Card            *CardConfig     `json:"card"`
				Schedule        *ScheduleConfig `json:"schedule"`
				UpdateLibrary   *struct {
					ID      string    `json:"id"`
					Name    *string   `json:"name"`
					Tasks   *[]string `json:"tasks"`
					Enabled *bool     `json:"enabled"`
					Count   *int      `json:"count"`
				} `json:"updateLibrary"`
				AddLibrary *struct {
					Name string `json:"name"`
				} `json:"addLibrary"`
				DeleteLibraryID *string `json:"deleteLibraryId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
				return
			}

			store.mu.Lock()
			if req.AddLibrary != nil {
				name := strings.TrimSpace(req.AddLibrary.Name)
				if name == "" {
					name = fmt.Sprintf("%d号库", len(store.state.Libraries)+1)
				}
				id := fmt.Sprintf("lib-%d", time.Now().UnixNano())
				store.state.Libraries = append(store.state.Libraries, TaskLibrary{
					ID:        id,
					Name:      name,
					Tasks:     []string{},
					Remaining: []string{},
					Round:     1,
					Enabled:   true,
					Count:     0,
				})
				store.state.ActiveLibraryID = id
			}
			if req.DeleteLibraryID != nil && len(store.state.Libraries) > 1 {
				delID := *req.DeleteLibraryID
				filtered := make([]TaskLibrary, 0, len(store.state.Libraries))
				for _, l := range store.state.Libraries {
					if l.ID != delID {
						filtered = append(filtered, l)
					}
				}
				if len(filtered) > 0 {
					store.state.Libraries = filtered
					if store.state.ActiveLibraryID == delID {
						store.state.ActiveLibraryID = filtered[0].ID
					}
				}
			}
			if req.UpdateLibrary != nil {
				if lib := store.findLibraryLocked(req.UpdateLibrary.ID); lib != nil {
					if req.UpdateLibrary.Name != nil {
						if nm := strings.TrimSpace(*req.UpdateLibrary.Name); nm != "" {
							lib.Name = nm
						}
					}
					if req.UpdateLibrary.Tasks != nil {
						updateLibraryTasks(lib, *req.UpdateLibrary.Tasks)
					}
					if req.UpdateLibrary.Enabled != nil {
						lib.Enabled = *req.UpdateLibrary.Enabled
					}
					if req.UpdateLibrary.Count != nil {
						c := *req.UpdateLibrary.Count
						if c < 0 {
							c = 0
						}
						lib.Count = c
					}
				}
			}
			if req.ActiveLibraryID != nil {
				if lib := store.findLibraryLocked(*req.ActiveLibraryID); lib != nil {
					store.state.ActiveLibraryID = lib.ID
				}
			}
			if req.NoRepeat != nil {
				store.state.NoRepeat = *req.NoRepeat
			}
			if req.Card != nil {
				store.state.Card = *req.Card
			}
			if req.Schedule != nil {
				lastDate := store.state.Schedule.LastRunDate
				lastTime := store.state.Schedule.LastRunTime
				lastSum := store.state.Schedule.LastRunSummary
				store.state.Schedule = *req.Schedule
				if store.state.Schedule.Count <= 0 {
					store.state.Schedule.Count = 20
				}
				if store.state.Schedule.Time == "" {
					store.state.Schedule.Time = "00:00"
				}
				store.state.Schedule.LastRunDate = lastDate
				store.state.Schedule.LastRunTime = lastTime
				store.state.Schedule.LastRunSummary = lastSum
			}
			_ = store.saveLocked()
			st := store.state
			store.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st})

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		}
	})

	mux.HandleFunc("/api/draw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		var req struct {
			LibraryID string `json:"libraryId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		store.mu.Lock()
		libID := req.LibraryID
		if libID == "" {
			libID = store.state.ActiveLibraryID
		}
		lib := store.findLibraryLocked(libID)
		task, ok := store.drawOneFromLibLocked(lib)
		_ = store.saveLocked()
		st := store.state
		store.mu.Unlock()

		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "当前任务库没有可抽取的任务", "state": st})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task, "state": st})
	})

	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		var req struct {
			LibraryID string `json:"libraryId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		store.mu.Lock()
		libID := req.LibraryID
		if libID == "" {
			libID = store.state.ActiveLibraryID
		}
		if lib := store.findLibraryLocked(libID); lib != nil {
			lib.Remaining = shuffleStrings(lib.Tasks)
			lib.Round = 1
		}
		_ = store.saveLocked()
		st := store.state
		store.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st})
	})

	mux.HandleFunc("/api/batch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		var req struct {
			Count     int    `json:"count"`
			Date      string `json:"date"`
			Suffix    string `json:"suffix"`
			LibraryID string `json:"libraryId"` // empty = all enabled libraries
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, err := store.runBatch(req.Count, req.Date, req.Suffix, req.LibraryID, "手动批量")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		store.mu.Lock()
		res["state"] = store.state
		store.mu.Unlock()
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("/api/save", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
		var req struct {
			ImageBase64 string `json:"imageBase64"`
			Date        string `json:"date"`
			Suffix      string `json:"suffix"`
			LibraryName string `json:"libraryName"`
			FileName    string `json:"fileName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
			return
		}
		b64 := req.ImageBase64
		if idx := strings.Index(b64, ","); idx != -1 {
			b64 = b64[idx+1:]
		}
		imgBytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(imgBytes) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid image data"})
			return
		}

		dateFolder := formatDateFolder(req.Date, req.Suffix)
		libFolder := sanitizeSegment(req.LibraryName)

		fileName := sanitizeSegment(req.FileName)
		if fileName == "" {
			fileName = time.Now().Format("150405") + ".png"
		}
		if !strings.HasSuffix(strings.ToLower(fileName), ".png") {
			fileName += ".png"
		}

		var targetDir string
		var relPath string
		if libFolder != "" {
			targetDir = filepath.Join(absOutput, dateFolder, libFolder)
			relPath = filepath.ToSlash(filepath.Join(dateFolder, libFolder, fileName))
		} else {
			targetDir = filepath.Join(absOutput, dateFolder)
			relPath = filepath.ToSlash(filepath.Join(dateFolder, fileName))
		}
		if !strings.HasPrefix(targetDir, absOutput) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid folder path"})
			return
		}
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		targetFile := filepath.Join(targetDir, fileName)
		if err := os.WriteFile(targetFile, imgBytes, 0o644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"path": relPath,
		})
	})

	log.Printf("listening on %s (output=%s, data=%s, fonts=%s)", addr, absOutput, absData, absFonts)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
