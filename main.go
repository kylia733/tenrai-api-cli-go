// arya - CLI anime ringkas berbasis Tenrai API (mirror Jikan v4).
// Hanya memakai standard library Go (butuh Go 1.18+ karena generics).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	version = "1.1.0"
	baseURL = "https://api.tenrai.org/v1"
	minGap  = 350 * time.Millisecond // Jikan-style rate limit ~3 req/detik
)

var (
	httpClient = &http.Client{Timeout: 15 * time.Second}
	lastReq    time.Time
	useColor   = true
	errDone    = errors.New("done") // sentinel: output --json sudah dicetak
)

// ───────────────────────────── Model ─────────────────────────────

type Pagination struct {
	CurrentPage     int  `json:"current_page"`
	LastVisiblePage int  `json:"last_visible_page"`
	HasNextPage     bool `json:"has_next_page"`
}

type Named struct {
	Name string `json:"name"`
}

type Anime struct {
	MalID    int      `json:"mal_id"`
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Type     string   `json:"type"`
	Episodes *int     `json:"episodes"`
	Status   string   `json:"status"`
	Score    *float64 `json:"score"`
	Synopsis *string  `json:"synopsis"`
	Genres   []Named  `json:"genres"`
	Studios  []Named  `json:"studios"`
}

type CharEntry struct {
	Character struct {
		Name string `json:"name"`
	} `json:"character"`
	Role string `json:"role"`
}

type Rec struct {
	Entry struct {
		MalID int    `json:"mal_id"`
		Title string `json:"title"`
	} `json:"entry"`
	Votes int `json:"votes"`
}

type RelEntry struct {
	MalID int    `json:"mal_id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
}

type Relation struct {
	Relation string     `json:"relation"`
	Entry    []RelEntry `json:"entry"`
}

type Episode struct {
	MalID  int    `json:"mal_id"`
	Title  string `json:"title"`
	Filler bool   `json:"filler"`
	Recap  bool   `json:"recap"`
}

type Genre struct {
	MalID int    `json:"mal_id"`
	Name  string `json:"name"`
}

// ───────────────────────────── Opsi / flag ─────────────────────────────

type opts struct {
	json, noColor, sfw, title bool
	limit, page               int
	typ, filter, day          string
}

func (o *opts) lim(def int) int {
	if o.limit > 0 {
		return o.limit
	}
	return def
}

// apiLim: untuk endpoint yang menerima parameter limit (maks 25).
func (o *opts) apiLim(def int) int {
	if l := o.lim(def); l < 25 {
		return l
	}
	return 25
}

func newFlagSet(o *opts) *flag.FlagSet {
	fs := flag.NewFlagSet("arya", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // pesan error flag ditangani di main
	fs.Usage = func() {}
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.noColor, "no-color", false, "")
	fs.BoolVar(&o.sfw, "sfw", false, "")
	fs.BoolVar(&o.title, "title", false, "")
	fs.IntVar(&o.limit, "limit", 0, "")
	fs.IntVar(&o.limit, "n", 0, "")
	fs.IntVar(&o.page, "page", 1, "")
	fs.StringVar(&o.typ, "type", "", "")
	fs.StringVar(&o.filter, "filter", "", "")
	fs.StringVar(&o.day, "day", "", "")
	return fs
}

// parseArgs mengizinkan flag di mana saja: `arya search Naruto --limit 3`.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// ===================== BANNER (EDIT DI SINI) =====================
// Ganti isi raw string `banner` dengan ASCII art kamu.
//   - Kosongkan (const banner = "") kalau tidak mau banner.
//   - Jangan pakai karakter backtick di dalam art (merusak raw string).
//   - bannerColor memakai kode warna ANSI: 31 merah, 32 hijau, 33 kuning,
//     34 biru, 35 magenta, 36 cyan, 97 putih.
//
// Banner tampil di `help` dan `version`, hanya jika output adalah terminal.
const (
	bannerColor = "36"
	tagline     = "Tenrai Anime CLI"
	banner      = `
   ░███    ░█████████  ░██     ░██    ░███    
  ░██░██   ░██     ░██  ░██   ░██    ░██░██   
 ░██  ░██  ░██     ░██   ░██ ░██    ░██  ░██  
░█████████ ░█████████     ░████    ░█████████ 
░██    ░██ ░██   ░██       ░██     ░██    ░██ 
░██    ░██ ░██    ░██      ░██     ░██    ░██ 
░██    ░██ ░██     ░██     ░██     ░██    ░██ 
`
)

func printBanner(w *os.File) {
	if banner == "" {
		return
	}
	fi, err := w.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return // bukan terminal (dipipe/redirect): jangan cetak banner
	}
	if os.Getenv("NO_COLOR") != "" {
		fmt.Fprint(w, banner)
	} else {
		fmt.Fprintf(w, "\033[%sm%s\033[0m\n", bannerColor, banner)
	}
	if tagline != "" {
		fmt.Fprintln(w, "  "+tagline)
		fmt.Fprintln(w)
	}
}

// home: layar awal saat program dijalankan tanpa argumen.
func home() {
	printBanner(os.Stdout)
	fmt.Printf(`ARYA v%s - CLI anime berbasis Tenrai API.

Mulai   : arya search <judul>
Bantuan : arya help
`, version)
}

func usage(w *os.File) {
	fmt.Fprintf(w, `ARYA v%s - CLI anime (Tenrai API)

PEMAKAIAN
  arya <perintah> [argumen] [flag]

PERINTAH
  search, s <judul>          Cari anime
  top, t                     Anime teratas
  info, i <id|judul>         Info singkat
  characters, c <id|judul>   Karakter
  recs <id|judul>            Rekomendasi serupa
  relations, rel <id|judul>  Sekuel / prekuel / spin-off
  episodes, eps <id|judul>   Daftar episode
  season [now|upcoming|<tahun> <musim>]
                             Anime per musim (winter/spring/summer/fall)
  schedule                   Jadwal tayang (default: hari ini)
  random                     Anime acak
  genres                     Daftar genre

FLAG
  -n, --limit N      Jumlah hasil (maks 25)
  --page N           Halaman berikutnya
  --type T           tv, movie, ova, special, ona, music
  --sfw              Sembunyikan konten dewasa
  --title            Baca argumen sebagai judul (mis. anime "86")
  --json             Output JSON mentah
  --no-color         Matikan warna
  --filter F         (top) airing|upcoming|bypopularity|favorite
  --day D            (schedule) monday..sunday|other|unknown

CONTOH
  arya search One Piece -n 5
  arya top --filter airing
  arya info Frieren
  arya season 2024 spring
`, version)
}

// ───────────────────────────── HTTP ─────────────────────────────

func apiErr(status int, body []byte) error {
	var e struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = e.Message
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	if status == http.StatusNotFound && msg == "" {
		msg = "tidak ditemukan"
	}
	return fmt.Errorf("API mengembalikan status %d: %s", status, msg)
}

const maxTries = 3

func fetch(path string, q url.Values) ([]byte, error) {
	u := baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	// Jeda antar percobaan, tapi tidak setelah percobaan terakhir.
	pause := func(attempt int, d time.Duration) {
		if attempt < maxTries-1 {
			time.Sleep(d)
		}
	}

	var lastErr error
	for attempt := 0; attempt < maxTries; attempt++ {
		if d := minGap - time.Since(lastReq); d > 0 {
			time.Sleep(d)
		}
		lastReq = time.Now()

		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "arya-cli/"+version)
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("gagal terhubung ke Tenrai API: %w", err)
			pause(attempt, time.Second<<attempt)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("gagal membaca respons: %w", readErr)
			pause(attempt, time.Second<<attempt)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = apiErr(resp.StatusCode, body)
			wait := time.Second << attempt
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 && ra <= 30 {
				wait = time.Duration(ra) * time.Second
			}
			pause(attempt, wait)
		default:
			return nil, apiErr(resp.StatusCode, body)
		}
	}
	return nil, lastErr
}

func printRaw(b []byte) {
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") == nil {
		fmt.Println(buf.String())
		return
	}
	fmt.Println(string(b))
}

func getList[T any](o *opts, path string, q url.Values) ([]T, *Pagination, error) {
	b, err := fetch(path, q)
	if err != nil {
		return nil, nil, err
	}
	if o.json {
		printRaw(b)
		return nil, nil, errDone
	}
	var r struct {
		Data       []T         `json:"data"`
		Pagination *Pagination `json:"pagination"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, nil, fmt.Errorf("gagal membaca data JSON: %w", err)
	}
	return r.Data, r.Pagination, nil
}

func getOne[T any](o *opts, path string) (T, error) {
	var zero T
	b, err := fetch(path, nil)
	if err != nil {
		return zero, err
	}
	if o.json {
		printRaw(b)
		return zero, errDone
	}
	var r struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return zero, fmt.Errorf("gagal membaca data JSON: %w", err)
	}
	return r.Data, nil
}

// ───────────────────────────── Helper tampilan ─────────────────────────────

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func say(o *opts, format string, a ...any) {
	if !o.json {
		fmt.Printf(format, a...)
	}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func names(ns []Named) string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return strings.Join(out, ", ")
}

func scoreStr(p *float64) string {
	if p == nil || *p == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.2f", *p)
}

func epsStr(p *int) string {
	if p == nil {
		return "? eps"
	}
	return fmt.Sprintf("%d eps", *p)
}

// shorten merapikan spasi lalu memotong teks maksimal n karakter.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "..."
}

func wrap(s string, width int, indent string) string {
	var sb strings.Builder
	sb.WriteString(indent)
	line := 0
	for _, w := range strings.Fields(s) {
		n := len([]rune(w))
		if line > 0 && line+1+n > width {
			sb.WriteString("\n" + indent)
			line = 0
		}
		if line > 0 {
			sb.WriteByte(' ')
			line++
		}
		sb.WriteString(w)
		line += n
	}
	return sb.String()
}

func heading(o *opts, format string, a ...any) {
	if o.json {
		return
	}
	fmt.Printf("%s\n\n", paint("1", fmt.Sprintf(format, a...)))
}

func footer(o *opts, pg *Pagination) {
	if pg == nil || pg.LastVisiblePage == 0 {
		return
	}
	cur := pg.CurrentPage
	if cur == 0 {
		cur = o.page
	}
	msg := fmt.Sprintf("Halaman %d dari %d", cur, pg.LastVisiblePage)
	if pg.HasNextPage {
		msg += fmt.Sprintf(" (berikutnya: --page %d)", cur+1)
	}
	fmt.Println(paint("2", msg))
}

func printAnimes(o *opts, list []Anime, pg *Pagination) {
	if len(list) == 0 {
		fmt.Println("Tidak ada anime yang ditemukan.")
		return
	}
	offset := (o.page - 1) * o.apiLim(10)
	for i, a := range list {
		fmt.Printf("%2d. %s %s\n", offset+i+1, paint("1", a.Title), paint("2", fmt.Sprintf("[ID %d]", a.MalID)))

		meta := []string{"Skor " + scoreStr(a.Score)}
		if a.Type != "" {
			meta = append(meta, a.Type)
		}
		meta = append(meta, epsStr(a.Episodes))
		if a.Status != "" {
			meta = append(meta, a.Status)
		}
		fmt.Printf("    %s\n", strings.Join(meta, " | "))
	}
	footer(o, pg)
}

func printDetail(a Anime) {
	fmt.Println(paint("1;36", a.Title))

	meta := []string{}
	if a.Type != "" {
		meta = append(meta, a.Type)
	}
	meta = append(meta, epsStr(a.Episodes))
	if a.Status != "" {
		meta = append(meta, a.Status)
	}
	meta = append(meta, "Skor "+scoreStr(a.Score))
	fmt.Println("  " + strings.Join(meta, " | "))

	if g := names(a.Genres); g != "" {
		fmt.Println("  Genre : " + g)
	}
	if s := names(a.Studios); s != "" {
		fmt.Println("  Studio: " + s)
	}
	if syn := shorten(str(a.Synopsis), 300); syn != "" {
		fmt.Println()
		fmt.Println(wrap(syn, 80, "  "))
	}
	fmt.Println()
	fmt.Printf("  %s %s\n", paint("2", fmt.Sprintf("[ID %d]", a.MalID)), a.URL)
}

// ───────────────────────────── Perintah ─────────────────────────────

func resolveID(o *opts, pos []string) (int, error) {
	if len(pos) == 0 {
		return 0, errors.New("butuh ID MAL atau judul anime")
	}
	if len(pos) == 1 && !o.title {
		if id, err := strconv.Atoi(pos[0]); err == nil && id > 0 {
			return id, nil
		}
	}
	title := strings.Join(pos, " ")
	q := url.Values{}
	q.Set("q", title)
	q.Set("limit", "1")
	setIf(q, "type", o.typ)
	if o.sfw {
		q.Set("sfw", "true")
	}
	oo := *o
	oo.json = false
	list, _, err := getList[Anime](&oo, "/anime", q)
	if err != nil {
		return 0, err
	}
	if len(list) == 0 {
		return 0, fmt.Errorf("anime '%s' tidak ditemukan", title)
	}
	say(o, "Hasil terpilih: %s (ID %d)\n\n", list[0].Title, list[0].MalID)
	return list[0].MalID, nil
}

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func cmdSearch(o *opts, pos []string) error {
	if len(pos) == 0 {
		return errors.New("kata kunci kosong. Contoh: arya search One Piece")
	}
	query := strings.Join(pos, " ")
	q := url.Values{}
	q.Set("q", query)
	q.Set("page", strconv.Itoa(o.page))
	q.Set("limit", strconv.Itoa(o.apiLim(10)))
	setIf(q, "type", o.typ)
	if o.sfw {
		q.Set("sfw", "true")
	}
	heading(o, "Hasil pencarian: %s", query)
	list, pg, err := getList[Anime](o, "/anime", q)
	if err != nil {
		return err
	}
	printAnimes(o, list, pg)
	return nil
}

func cmdTop(o *opts, _ []string) error {
	q := url.Values{}
	q.Set("page", strconv.Itoa(o.page))
	q.Set("limit", strconv.Itoa(o.apiLim(10)))
	setIf(q, "type", o.typ)
	setIf(q, "filter", o.filter)
	if o.sfw {
		q.Set("sfw", "true")
	}
	if o.filter != "" {
		heading(o, "Top anime (%s)", o.filter)
	} else {
		heading(o, "Top anime")
	}
	list, pg, err := getList[Anime](o, "/top/anime", q)
	if err != nil {
		return err
	}
	printAnimes(o, list, pg)
	return nil
}

func cmdInfo(o *opts, pos []string) error {
	id, err := resolveID(o, pos)
	if err != nil {
		return err
	}
	a, err := getOne[Anime](o, fmt.Sprintf("/anime/%d", id))
	if err != nil {
		return err
	}
	printDetail(a)
	return nil
}

func cmdRandom(o *opts, _ []string) error {
	heading(o, "Anime acak")
	a, err := getOne[Anime](o, "/random/anime")
	if err != nil {
		return err
	}
	printDetail(a)
	return nil
}

func cmdCharacters(o *opts, pos []string) error {
	id, err := resolveID(o, pos)
	if err != nil {
		return err
	}
	list, _, err := getList[CharEntry](o, fmt.Sprintf("/anime/%d/characters", id), nil)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Tidak ada data karakter.")
		return nil
	}
	if n := o.lim(5); len(list) > n {
		list = list[:n]
	}
	for i, c := range list {
		fmt.Printf("%2d. %s %s\n", i+1, c.Character.Name, paint("2", "["+c.Role+"]"))
	}
	return nil
}

func cmdRecs(o *opts, pos []string) error {
	id, err := resolveID(o, pos)
	if err != nil {
		return err
	}
	list, _, err := getList[Rec](o, fmt.Sprintf("/anime/%d/recommendations", id), nil)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Belum ada rekomendasi.")
		return nil
	}
	if n := o.lim(5); len(list) > n {
		list = list[:n]
	}
	for i, r := range list {
		fmt.Printf("%2d. %s %s\n", i+1, r.Entry.Title,
			paint("2", fmt.Sprintf("(%d vote, ID %d)", r.Votes, r.Entry.MalID)))
	}
	return nil
}

func cmdRelations(o *opts, pos []string) error {
	id, err := resolveID(o, pos)
	if err != nil {
		return err
	}
	list, _, err := getList[Relation](o, fmt.Sprintf("/anime/%d/relations", id), nil)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Tidak ada relasi.")
		return nil
	}
	for _, r := range list {
		fmt.Println(paint("1;36", r.Relation))
		for _, e := range r.Entry {
			fmt.Printf("  - %s %s\n", e.Name, paint("2", fmt.Sprintf("[%s, ID %d]", e.Type, e.MalID)))
		}
	}
	return nil
}

func cmdEpisodes(o *opts, pos []string) error {
	id, err := resolveID(o, pos)
	if err != nil {
		return err
	}
	if o.limit > 0 {
		fmt.Fprintln(os.Stderr, "Catatan: --limit tidak berlaku untuk episodes; satu halaman penuh ditampilkan. Gunakan --page untuk lanjut.")
	}
	q := url.Values{}
	q.Set("page", strconv.Itoa(o.page))
	list, pg, err := getList[Episode](o, fmt.Sprintf("/anime/%d/episodes", id), q)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Belum ada data episode.")
		return nil
	}
	for _, e := range list {
		tags := ""
		if e.Filler {
			tags += " [filler]"
		}
		if e.Recap {
			tags += " [recap]"
		}
		fmt.Printf("%4d. %s%s\n", e.MalID, e.Title, paint("33", tags))
	}
	footer(o, pg)
	return nil
}

func cmdSeason(o *opts, pos []string) error {
	q := url.Values{}
	q.Set("page", strconv.Itoa(o.page))
	q.Set("limit", strconv.Itoa(o.apiLim(10)))
	setIf(q, "filter", o.typ)
	if o.sfw {
		q.Set("sfw", "true")
	}

	var path, label string
	switch {
	case len(pos) == 0 || pos[0] == "now":
		path, label = "/seasons/now", "musim ini"
	case pos[0] == "upcoming":
		path, label = "/seasons/upcoming", "musim depan"
	case len(pos) == 2:
		year, err := strconv.Atoi(pos[0])
		season := strings.ToLower(pos[1])
		if err != nil || year < 1917 {
			return errors.New("tahun tidak valid. Contoh: arya season 2024 spring")
		}
		switch season {
		case "winter", "spring", "summer", "fall":
		default:
			return errors.New("musim harus winter, spring, summer, atau fall")
		}
		path, label = fmt.Sprintf("/seasons/%d/%s", year, season), fmt.Sprintf("%s %d", season, year)
	default:
		return errors.New("format: arya season [now|upcoming|<tahun> <musim>]")
	}

	heading(o, "Anime %s", label)
	list, pg, err := getList[Anime](o, path, q)
	if err != nil {
		return err
	}
	printAnimes(o, list, pg)
	return nil
}

func cmdSchedule(o *opts, _ []string) error {
	day := strings.ToLower(o.day)
	label := day
	if day == "" {
		// WIB = UTC+7, tanpa DST. FixedZone tidak butuh database zona waktu (aman di Windows).
		// Catatan: API mengelompokkan jadwal berdasarkan hari siaran di Jepang (JST).
		wib := time.FixedZone("WIB", 7*3600)
		day = strings.ToLower(time.Now().In(wib).Weekday().String())
		label = day + " (hari ini, WIB)"
	}
	switch day {
	case "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "other", "unknown":
	default:
		return errors.New("--day harus monday..sunday, other, atau unknown")
	}
	q := url.Values{}
	q.Set("filter", day)
	q.Set("page", strconv.Itoa(o.page))
	q.Set("limit", strconv.Itoa(o.apiLim(10)))
	if o.sfw {
		q.Set("sfw", "true")
	}
	heading(o, "Jadwal tayang: %s", label)
	list, pg, err := getList[Anime](o, "/schedules", q)
	if err != nil {
		return err
	}
	printAnimes(o, list, pg)
	return nil
}

func cmdGenres(o *opts, _ []string) error {
	list, _, err := getList[Genre](o, "/genres/anime", nil)
	if err != nil {
		return err
	}
	for _, g := range list {
		fmt.Printf("  %3d  %s\n", g.MalID, g.Name)
	}
	return nil
}

// ───────────────────────────── main ─────────────────────────────

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func main() {
	if len(os.Args) < 2 {
		home()
		return
	}

	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		usage(os.Stdout)
		return
	}
	if cmd == "version" || cmd == "--version" {
		printBanner(os.Stdout)
		fmt.Println("ARYA v" + version)
		return
	}

	commands := map[string]func(*opts, []string) error{
		"search": cmdSearch, "s": cmdSearch,
		"top": cmdTop, "t": cmdTop,
		"info": cmdInfo, "i": cmdInfo,
		"characters": cmdCharacters, "c": cmdCharacters,
		"recs":      cmdRecs,
		"relations": cmdRelations, "rel": cmdRelations,
		"episodes": cmdEpisodes, "eps": cmdEpisodes,
		"season":   cmdSeason,
		"schedule": cmdSchedule,
		"random":   cmdRandom,
		"genres":   cmdGenres,
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "Perintah '%s' tidak dikenal.\n\n", cmd)
		usage(os.Stderr)
		os.Exit(1)
	}

	o := &opts{}
	fs := newFlagSet(o)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\nGunakan 'arya help' untuk daftar flag.\n", err)
		os.Exit(2)
	}
	if o.page < 1 {
		o.page = 1
	}
	if o.limit < 0 {
		fmt.Fprintln(os.Stderr, "Error: --limit harus bernilai positif")
		os.Exit(2)
	}
	switch cmd {
	case "search", "s", "top", "t", "season", "schedule":
		if o.limit > 25 {
			fmt.Fprintln(os.Stderr, "Catatan: API membatasi maksimal 25 hasil per halaman; memakai 25. Gunakan --page untuk hasil berikutnya.")
		}
	}

	tty := isTerminal()
	useColor = tty && !o.noColor && !o.json && os.Getenv("NO_COLOR") == ""

	if err := run(o, pos); err != nil && !errors.Is(err, errDone) {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}