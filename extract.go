package main

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxTextRead = 2 << 20  // сколько читаем из текстового файла
	maxZipRead  = 12 << 20 // сколько распаковываем из docx/xlsx/pptx
	maxPDFSize  = 60 << 20
)

// contentEligible — читаем ли мы содержимое файла.
func contentEligible(e *Entry) bool {
	if e.Dir {
		return false
	}
	switch {
	case textExts[e.Ext]:
		return e.Size <= 64<<20
	case zipDocExts[e.Ext]:
		return e.Size <= 80<<20
	case e.Ext == "pdf":
		return e.Size <= maxPDFSize
	case e.Ext == "" && textNames[e.Low]:
		return e.Size <= 4<<20
	}
	return false
}

func readHead(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)))
	return b, err
}

var cp1251hi = func() [64]rune {
	var t [64]rune
	for i := range t {
		t[i] = rune(0x0410 + i)
	}
	return t
}()

func decodeText(b []byte) string {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		le := b[0] == 0xFF
		b = b[2:]
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if le {
				u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
			} else {
				u = append(u, uint16(b[i+1])|uint16(b[i])<<8)
			}
		}
		return string(utf16.Decode(u))
	}
	probe := b
	if len(probe) > 4096 {
		probe = probe[:4096]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return "" // бинарный файл
	}
	if utf8.Valid(b) || (len(b) > 4 && utf8.Valid(b[:len(b)-3])) {
		return string(b)
	}
	// cp1251
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c >= 0xC0:
			sb.WriteRune(cp1251hi[c-0xC0])
		case c == 0xA8:
			sb.WriteRune('Ё')
		case c == 0xB8:
			sb.WriteRune('ё')
		default:
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// extractText возвращает текст файла (до maxChars символов), либо "".
func extractText(path, ext string, maxChars int) string {
	defer func() { _ = recover() }()
	var s string
	switch {
	case ext == "pdf":
		s = extractPDF(path, maxChars)
	case zipDocExts[ext]:
		s = extractZipDoc(path, ext, maxChars)
	default:
		n := maxTextRead
		if maxChars > 0 && maxChars*4 < n {
			n = maxChars * 4
		}
		b, err := readHead(path, n)
		if err != nil {
			return ""
		}
		s = decodeText(b)
		if ext == "html" || ext == "htm" || ext == "xml" || ext == "fb2" || ext == "svg" {
			if maxChars > 0 {
				// для превью/сниппета HTML показываем как текст
			}
			s = html.UnescapeString(stripXML([]byte(s), true))
		}
	}
	if maxChars > 0 && len(s) > maxChars {
		s = s[:maxChars]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	return s
}

// ---------- XML / OOXML / ODF ----------

var officeSep = map[string]bool{
	"w:p": true, "a:p": true, "text:p": true, "text:h": true, "si": true, "row": true,
	"w:br": true, "w:tab": true, "w:tc": true, "table:table-cell": true, "text:line-break": true,
	"text:tab": true, "a:br": true, "text:s": true, "w:cr": true,
}

func stripXML(b []byte, generic bool) string {
	var sb strings.Builder
	sb.Grow(len(b) / 3)
	n := len(b)
	skipUntil := ""
	for i := 0; i < n; {
		if b[i] != '<' {
			j := bytes.IndexByte(b[i:], '<')
			if j < 0 {
				j = n - i
			}
			if skipUntil == "" {
				sb.Write(b[i : i+j])
			}
			i += j
			continue
		}
		j := bytes.IndexByte(b[i:], '>')
		if j < 0 {
			break
		}
		tag := b[i+1 : i+j]
		i += j + 1
		if len(tag) == 0 {
			continue
		}
		closing := tag[0] == '/'
		name := tag
		if closing {
			name = name[1:]
		}
		if k := bytes.IndexAny(name, " \t\r\n/"); k >= 0 {
			name = name[:k]
		}
		lname := strings.ToLower(string(name))
		if generic {
			if skipUntil != "" {
				if closing && lname == skipUntil {
					skipUntil = ""
				}
				continue
			}
			if !closing && (lname == "script" || lname == "style") && tag[len(tag)-1] != '/' {
				skipUntil = lname
				continue
			}
			sb.WriteByte(' ')
			continue
		}
		if officeSep[lname] && (closing || tag[len(tag)-1] == '/' || lname == "w:br" || lname == "w:tab") {
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

var slideNum = regexp.MustCompile(`(\d+)`)

func extractZipDoc(path, ext string, maxChars int) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	var parts []*zip.File
	for _, f := range zr.File {
		n := f.Name
		switch ext {
		case "docx":
			if n == "word/document.xml" || strings.HasPrefix(n, "word/header") || strings.HasPrefix(n, "word/footnotes") {
				parts = append(parts, f)
			}
		case "xlsx":
			if n == "xl/sharedStrings.xml" {
				parts = append(parts, f)
			}
		case "pptx":
			if strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml") {
				parts = append(parts, f)
			}
		default: // odt ods odp
			if n == "content.xml" {
				parts = append(parts, f)
			}
		}
	}
	sort.Slice(parts, func(i, j int) bool {
		a, b := parts[i].Name, parts[j].Name
		na, nb := 0, 0
		if m := slideNum.FindString(a); m != "" {
			na, _ = strconv.Atoi(m)
		}
		if m := slideNum.FindString(b); m != "" {
			nb, _ = strconv.Atoi(m)
		}
		if na != nb {
			return na < nb
		}
		return a < b
	})
	var sb strings.Builder
	for _, f := range parts {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(rc, maxZipRead))
		rc.Close()
		sb.WriteString(html.UnescapeString(stripXML(b, false)))
		sb.WriteByte('\n')
		if maxChars > 0 && sb.Len() > maxChars {
			break
		}
	}
	return sb.String()
}

// ---------- PDF (упрощённый разбор: Flate-потоки, ObjStm, ToUnicode) ----------

type pdfObj struct {
	dict   string
	stream []byte
}

var (
	reObj     = regexp.MustCompile(`(?m)(\d+)\s+(\d+)\s+obj\b`)
	reRef     = regexp.MustCompile(`(\d+)\s+\d+\s+R`)
	rePageTyp = regexp.MustCompile(`/Type\s*/Page\b`)
	reLen     = regexp.MustCompile(`/Length\s+(\d+)(\s+\d+\s+R)?`)
)

func inflate(b []byte) []byte {
	r, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	defer r.Close()
	out, _ := io.ReadAll(io.LimitReader(r, 64<<20))
	return out
}

func extractPDF(path string, maxChars int) (res string) {
	defer func() {
		if recover() != nil {
			res = ""
		}
	}()
	st, err := os.Stat(path)
	if err != nil || st.Size() > maxPDFSize {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	objs := map[int]*pdfObj{}
	locs := reObj.FindAllSubmatchIndex(data, -1)
	for k, loc := range locs {
		num, _ := strconv.Atoi(string(data[loc[2]:loc[3]]))
		end := len(data)
		if k+1 < len(locs) {
			end = locs[k+1][0]
		}
		body := data[loc[1]:end]
		o := &pdfObj{}
		if si := bytes.Index(body, []byte("stream")); si >= 0 && (bytes.Contains(body[:si], []byte("<<"))) {
			o.dict = string(body[:si])
			s := body[si+6:]
			if len(s) > 0 && s[0] == '\r' {
				s = s[1:]
			}
			if len(s) > 0 && s[0] == '\n' {
				s = s[1:]
			}
			if ei := bytes.LastIndex(s, []byte("endstream")); ei >= 0 {
				s = s[:ei]
			}
			s = bytes.TrimRight(s, "\r\n")
			o.stream = s
			if strings.Contains(o.dict, "FlateDecode") {
				o.stream = inflate(s)
			}
		} else {
			if ei := bytes.Index(body, []byte("endobj")); ei >= 0 {
				body = body[:ei]
			}
			o.dict = string(body)
		}
		objs[num] = o
	}
	// объектные потоки
	for _, o := range objs {
		if strings.Contains(o.dict, "/ObjStm") && len(o.stream) > 0 {
			nm := regexp.MustCompile(`/N\s+(\d+)`).FindStringSubmatch(o.dict)
			fm := regexp.MustCompile(`/First\s+(\d+)`).FindStringSubmatch(o.dict)
			if nm == nil || fm == nil {
				continue
			}
			n, _ := strconv.Atoi(nm[1])
			first, _ := strconv.Atoi(fm[1])
			if first > len(o.stream) {
				continue
			}
			hdr := strings.Fields(string(o.stream[:first]))
			for i := 0; i+1 < len(hdr) && i/2 < n; i += 2 {
				on, _ := strconv.Atoi(hdr[i])
				off, _ := strconv.Atoi(hdr[i+1])
				endOff := len(o.stream) - first
				if i+3 < len(hdr) {
					endOff, _ = strconv.Atoi(hdr[i+3])
				}
				if first+off <= len(o.stream) && first+endOff <= len(o.stream) && off <= endOff {
					objs[on] = &pdfObj{dict: string(o.stream[first+off : first+endOff])}
				}
			}
		}
	}
	// страницы по порядку номеров объектов
	var nums []int
	for n, o := range objs {
		if rePageTyp.MatchString(o.dict) {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	fontCache := map[int]*pdfFont{}
	var sb strings.Builder
	for _, pn := range nums {
		page := objs[pn]
		fonts := pageFonts(objs, page, fontCache)
		for _, cn := range pageContents(objs, page) {
			c := objs[cn]
			if c == nil || len(c.stream) == 0 {
				continue
			}
			pdfContent(c.stream, fonts, &sb)
			sb.WriteByte('\n')
		}
		if maxChars > 0 && sb.Len() > maxChars {
			break
		}
		if sb.Len() > 8<<20 {
			break
		}
	}
	return sb.String()
}

func pageContents(objs map[int]*pdfObj, page *pdfObj) []int {
	i := strings.Index(page.dict, "/Contents")
	if i < 0 {
		return nil
	}
	rest := page.dict[i+9:]
	rest = strings.TrimLeft(rest, " \r\n\t")
	var seg string
	if strings.HasPrefix(rest, "[") {
		if e := strings.Index(rest, "]"); e > 0 {
			seg = rest[:e]
		}
	} else if m := reRef.FindString(rest); m != "" && strings.Index(rest, m) < 20 {
		seg = m
	}
	var out []int
	for _, m := range reRef.FindAllStringSubmatch(seg, -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// balanced возвращает «<<...>>» начиная с позиции s[i:] (s[i:] начинается с "<<").
func balanced(s string) string {
	depth := 0
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '<' && s[i+1] == '<' {
			depth++
			i++
		} else if s[i] == '>' && s[i+1] == '>' {
			depth--
			i++
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}

func pageFonts(objs map[int]*pdfObj, page *pdfObj, cache map[int]*pdfFont) map[string]*pdfFont {
	res := ""
	cur := page
	for hops := 0; hops < 6 && cur != nil && res == ""; hops++ {
		if i := strings.Index(cur.dict, "/Resources"); i >= 0 {
			rest := strings.TrimLeft(cur.dict[i+10:], " \r\n\t")
			if strings.HasPrefix(rest, "<<") {
				res = balanced(rest)
			} else if m := reRef.FindStringSubmatch(rest); m != nil {
				n, _ := strconv.Atoi(m[1])
				if o := objs[n]; o != nil {
					res = o.dict
				}
			}
			break
		}
		pm := regexp.MustCompile(`/Parent\s+(\d+)\s+\d+\s+R`).FindStringSubmatch(cur.dict)
		if pm == nil {
			break
		}
		pn, _ := strconv.Atoi(pm[1])
		cur = objs[pn]
	}
	out := map[string]*pdfFont{}
	fi := strings.Index(res, "/Font")
	if fi < 0 {
		return out
	}
	rest := strings.TrimLeft(res[fi+5:], " \r\n\t")
	fd := ""
	if strings.HasPrefix(rest, "<<") {
		fd = balanced(rest)
	} else if m := reRef.FindStringSubmatch(rest); m != nil {
		n, _ := strconv.Atoi(m[1])
		if o := objs[n]; o != nil {
			fd = o.dict
		}
	}
	for _, m := range regexp.MustCompile(`/([^\s/<>\[\]()]+)\s+(\d+)\s+\d+\s+R`).FindAllStringSubmatch(fd, -1) {
		n, _ := strconv.Atoi(m[2])
		if f, ok := cache[n]; ok {
			out[m[1]] = f
			continue
		}
		f := loadFont(objs, objs[n])
		cache[n] = f
		out[m[1]] = f
	}
	return out
}

type pdfFont struct {
	cmap    map[uint32]string
	codeLen int
	type0   bool
}

func loadFont(objs map[int]*pdfObj, o *pdfObj) *pdfFont {
	f := &pdfFont{codeLen: 1}
	if o == nil {
		return f
	}
	f.type0 = strings.Contains(o.dict, "/Type0")
	if f.type0 {
		f.codeLen = 2
	}
	if m := regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`).FindStringSubmatch(o.dict); m != nil {
		n, _ := strconv.Atoi(m[1])
		if c := objs[n]; c != nil && len(c.stream) > 0 {
			f.cmap, f.codeLen = parseCMap(string(c.stream), f.codeLen)
		}
	}
	return f
}

var (
	reHex      = regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)
	reBfChar   = regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`)
	reBfRange  = regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`)
	reCodeSpan = regexp.MustCompile(`(?s)begincodespacerange\s*<([0-9A-Fa-f]+)>`)
)

func hexUni(h string) string {
	h = strings.Join(strings.Fields(h), "")
	if len(h)%4 != 0 {
		if len(h) <= 2 {
			v, _ := strconv.ParseUint(h, 16, 32)
			return string(rune(v))
		}
		return ""
	}
	var u []uint16
	for i := 0; i+4 <= len(h); i += 4 {
		v, _ := strconv.ParseUint(h[i:i+4], 16, 32)
		u = append(u, uint16(v))
	}
	return string(utf16.Decode(u))
}

func parseCMap(s string, defLen int) (map[uint32]string, int) {
	m := map[uint32]string{}
	codeLen := defLen
	if cs := reCodeSpan.FindStringSubmatch(s); cs != nil {
		codeLen = len(cs[1]) / 2
		if codeLen < 1 {
			codeLen = 1
		}
	}
	for _, blk := range reBfChar.FindAllStringSubmatch(s, -1) {
		hx := reHex.FindAllStringSubmatch(blk[1], -1)
		for i := 0; i+1 < len(hx); i += 2 {
			code, _ := strconv.ParseUint(strings.TrimSpace(hx[i][1]), 16, 32)
			m[uint32(code)] = hexUni(hx[i+1][1])
		}
	}
	for _, blk := range reBfRange.FindAllStringSubmatch(s, -1) {
		for _, line := range strings.Split(blk[1], "\n") {
			hx := reHex.FindAllStringSubmatch(line, -1)
			if len(hx) < 3 {
				if len(hx) == 3 {
					continue
				}
				continue
			}
			lo, _ := strconv.ParseUint(hx[0][1], 16, 32)
			hi, _ := strconv.ParseUint(hx[1][1], 16, 32)
			if hi < lo || hi-lo > 65535 {
				continue
			}
			if strings.Contains(line, "[") {
				for k := 0; k < len(hx)-2 && lo+uint64(k) <= hi; k++ {
					m[uint32(lo)+uint32(k)] = hexUni(hx[2+k][1])
				}
			} else {
				base := []rune(hexUni(hx[2][1]))
				if len(base) == 0 {
					continue
				}
				for c := lo; c <= hi; c++ {
					r := append([]rune(nil), base...)
					r[len(r)-1] += rune(c - lo)
					m[uint32(c)] = string(r)
				}
			}
		}
	}
	return m, codeLen
}

func (f *pdfFont) decode(b []byte, sb *strings.Builder) {
	if f == nil {
		f = &pdfFont{codeLen: 1}
	}
	if f.cmap == nil {
		if f.type0 {
			return
		}
		for _, c := range b {
			if c >= 32 && c < 127 {
				sb.WriteByte(c)
			} else if c >= 0xC0 {
				sb.WriteRune(rune(c)) // latin-1
			} else if c == 9 || c == 10 {
				sb.WriteByte(' ')
			}
		}
		return
	}
	n := f.codeLen
	if n < 1 {
		n = 1
	}
	for i := 0; i+n <= len(b); i += n {
		var code uint32
		for k := 0; k < n; k++ {
			code = code<<8 | uint32(b[i+k])
		}
		if s, ok := f.cmap[code]; ok {
			sb.WriteString(s)
		}
	}
}

// pdfContent разбирает контент-поток страницы и дописывает текст.
func pdfContent(s []byte, fonts map[string]*pdfFont, out *strings.Builder) {
	var cur *pdfFont
	var strs [][]byte // накопленные строки для TJ / Tj
	var lastName string
	var lastNum float64
	var inArr bool
	var arr strings.Builder
	n := len(s)
	flushStr := func(op string) {
		_ = op
	}
	_ = flushStr
	emit := func(b []byte) {
		if inArr {
			cur.decode(b, &arr)
		} else {
			cur.decode(b, out)
		}
	}
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == '(':
			depth := 1
			i++
			var b []byte
			for i < n && depth > 0 {
				ch := s[i]
				switch ch {
				case '\\':
					i++
					if i < n {
						e := s[i]
						switch e {
						case 'n':
							b = append(b, '\n')
						case 'r':
							b = append(b, '\r')
						case 't':
							b = append(b, '\t')
						case 'b', 'f':
						case '0', '1', '2', '3', '4', '5', '6', '7':
							v := int(e - '0')
							for k := 0; k < 2 && i+1 < n && s[i+1] >= '0' && s[i+1] <= '7'; k++ {
								i++
								v = v*8 + int(s[i]-'0')
							}
							b = append(b, byte(v))
						case '\r', '\n':
						default:
							b = append(b, e)
						}
					}
				case '(':
					depth++
					b = append(b, ch)
				case ')':
					depth--
					if depth > 0 {
						b = append(b, ch)
					}
				default:
					b = append(b, ch)
				}
				i++
			}
			emit(b)
			strs = append(strs, b)
		case c == '<' && i+1 < n && s[i+1] != '<':
			j := bytes.IndexByte(s[i:], '>')
			if j < 0 {
				return
			}
			hx := strings.Join(strings.Fields(string(s[i+1:i+j])), "")
			if len(hx)%2 == 1 {
				hx += "0"
			}
			b := make([]byte, 0, len(hx)/2)
			for k := 0; k+2 <= len(hx); k += 2 {
				v, _ := strconv.ParseUint(hx[k:k+2], 16, 8)
				b = append(b, byte(v))
			}
			emit(b)
			i += j + 1
		case c == '<':
			i += 2
		case c == '[':
			inArr = true
			arr.Reset()
			i++
		case c == ']':
			if inArr {
				out.WriteString(arr.String())
			}
			inArr = false
			i++
		case c == '/':
			j := i + 1
			for j < n && !isPDFDelim(s[j]) {
				j++
			}
			lastName = string(s[i+1 : j])
			i = j
		case c == '%':
			for i < n && s[i] != '\n' && s[i] != '\r' {
				i++
			}
		case c == '-' || c == '.' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < n && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			v, _ := strconv.ParseFloat(string(s[i:j]), 64)
			lastNum = v
			if inArr && v < -220 {
				arr.WriteByte(' ')
			}
			i = j
		case c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == 0 || c == '>' || c == '{' || c == '}':
			i++
		default:
			j := i
			for j < n && !isPDFDelim(s[j]) {
				j++
			}
			if j == i {
				i++
				continue
			}
			op := string(s[i:j])
			i = j
			switch op {
			case "Tf":
				cur = fonts[lastName]
			case "Td", "TD", "T*", "Tm", "ET", "'", "\"":
				out.WriteByte(' ')
				if op == "T*" || op == "ET" || (op == "Td" || op == "TD") && lastNum != 0 {
					out.WriteByte('\n')
				}
			}
			strs = strs[:0]
		}
	}
}

func isPDFDelim(c byte) bool {
	switch c {
	case ' ', '\n', '\r', '\t', 0, '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

// ---------- сниппеты ----------

func snippetAround(text string, toks []string, width int) string {
	low := norm(text)
	best := -1
	for _, t := range toks {
		if t == "" {
			continue
		}
		if i := strings.Index(low, t); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	// позиции в low и text могут расходиться из-за ToLower; ограничиваем по рунам
	start := best - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(text) {
		end = len(text)
	}
	if start > len(text) {
		start = len(text)
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	seg := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		seg = "…" + seg
	}
	if end < len(text) {
		seg += "…"
	}
	return seg
}

func headSnippet(text string, maxLines, maxChars int) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	var out []string
	total := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(l) > 90 {
			l = l[:90]
		}
		out = append(out, l)
		total += len(l)
		if len(out) >= maxLines || total > maxChars {
			break
		}
	}
	s := strings.Join(out, "\n")
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func baseNoExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}
