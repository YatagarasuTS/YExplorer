package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cat — категория файла для вкладок фильтра.
type Cat uint8

const (
	CatOther Cat = iota
	CatImage
	CatDoc
	CatCode
	CatMedia
	CatFolder
)

func set(list string) map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Fields(list) {
		m[s] = true
	}
	return m
}

var (
	imageExts = set("png jpg jpeg gif bmp webp svg ico tif tiff heic avif jfif")
	docExts   = set("doc docx pdf txt md rtf odt ods odp xls xlsx csv ppt pptx fb2 epub djvu pages numbers key tsv")
	codeExts  = set("go py pyw js jsx ts tsx mjs cjs vue svelte java kt kts c h cpp hpp cc cs php rb swift dart lua sh bash zsh bat cmd ps1 sql r scala pl rs html htm css scss less json xml yml yaml toml ini cfg conf gradle properties cmake tex ipynb sln csproj vbs asm")
	mediaExts = set("mp3 wav flac ogg m4a aac wma mp4 mkv avi mov wmv webm flv m4v mpg mpeg opus")
	// форматы, которые читаем как текст
	textExts = set("txt md markdown log csv tsv json xml yml yaml toml ini cfg conf env html htm css scss less js jsx ts tsx mjs cjs vue svelte py pyw go rs java kt kts c h cpp hpp cc cs php rb swift dart lua sh bash zsh bat cmd ps1 sql r scala pl tex srt vtt gradle properties cmake ipynb sln csproj vbs asm fb2 svg")
	// офисные форматы-архивы
	zipDocExts = set("docx xlsx pptx odt ods odp")
	ocrExts    = set("png jpg jpeg bmp tif tiff")
	// файлы без расширения, которые читаем как текст
	textNames = set("dockerfile makefile readme license changelog")
)

func catFor(ext string, dir bool) Cat {
	switch {
	case dir:
		return CatFolder
	case imageExts[ext]:
		return CatImage
	case codeExts[ext]:
		return CatCode
	case docExts[ext]:
		return CatDoc
	case mediaExts[ext]:
		return CatMedia
	}
	return CatOther
}

// norm — нижний регистр + ё→е.
func norm(s string) string {
	s = strings.ToLower(s)
	if strings.ContainsRune(s, 'ё') {
		s = strings.ReplaceAll(s, "ё", "е")
	}
	return s
}

// tokenize разбивает текст на слова (буквы/цифры), нижний регистр, 2..32 символа.
func tokenize(text string, emit func(string)) {
	var buf []rune
	flush := func() {
		if n := len(buf); n >= 2 && n <= 32 {
			emit(string(buf))
		}
		buf = buf[:0]
	}
	for i := 0; i < len(text); {
		c := text[i]
		if c < utf8.RuneSelf {
			i++
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				buf = append(buf, rune(c))
			case c >= 'A' && c <= 'Z':
				buf = append(buf, rune(c+32))
			default:
				if len(buf) > 0 {
					flush()
				}
			}
			continue
		}
		r, sz := utf8.DecodeRuneInString(text[i:])
		i += sz
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			r = unicode.ToLower(r)
			if r == 'ё' {
				r = 'е'
			}
			buf = append(buf, r)
		} else if len(buf) > 0 {
			flush()
		}
	}
	if len(buf) > 0 {
		flush()
	}
}

// ---- раскладка клавиатуры ----

var enRow = []rune("qwertyuiop[]asdfghjkl;'zxcvbnm,./`")
var ruRow = []rune("йцукенгшщзхъфывапролджэячсмитьбю.ё")
var en2ru, ru2en = map[rune]rune{}, map[rune]rune{}

func init() {
	for i, e := range enRow {
		en2ru[e] = ruRow[i]
		ru2en[ruRow[i]] = e
	}
}

// fixLayout: «ghbdtn» → «привет», «руддщ» → «hello». Пусто, если не применимо.
func fixLayout(q string) string {
	low := strings.ToLower(q)
	hasLat, hasCyr := false, false
	for _, r := range low {
		switch {
		case r >= 'a' && r <= 'z':
			hasLat = true
		case r >= 'а' && r <= 'я' || r == 'ё':
			hasCyr = true
		}
	}
	if hasLat == hasCyr {
		return ""
	}
	m := en2ru
	if hasCyr {
		m = ru2en
	}
	var sb strings.Builder
	changed := false
	for _, r := range low {
		if t, ok := m[r]; ok {
			sb.WriteRune(t)
			changed = true
		} else {
			sb.WriteRune(r)
		}
	}
	if !changed {
		return ""
	}
	return sb.String()
}

// ---- расстояние Дамерау–Левенштейна (OSA) ----

func osa(a, b []rune, limit int) int {
	la, lb := len(a), len(b)
	if la-lb > limit || lb-la > limit {
		return limit + 1
	}
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev2 := make([]int, lb+1)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prev2[j-2] + 1; t < v {
					v = t
				}
			}
			cur[j] = v
			if v < rowMin {
				rowMin = v
			}
		}
		if rowMin > limit {
			return limit + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// maxTypos — сколько опечаток допускаем для слова данной длины.
func maxTypos(n int) int {
	switch {
	case n <= 2:
		return 0
	case n <= 4:
		return 1
	case n <= 6:
		return 1
	}
	return 2
}

func splitWords(low string) []string {
	var out []string
	start := -1
	for i, r := range low {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			out = append(out, low[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, low[start:])
	}
	return out
}
