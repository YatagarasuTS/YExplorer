package com.yexplorer.app;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.Charset;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;
import java.util.zip.ZipEntry;
import java.util.zip.ZipFile;

/** Чтение текста из файлов: txt/код (utf-8, utf-16, cp1251), docx/xlsx/pptx/odf. */
public final class Extract {
    private Extract() {}

    static final int MAX_TEXT_READ = 512 * 1024;
    static final int MAX_ZIP_PART = 6 * 1024 * 1024;

    static byte[] readHead(File f, int max) throws IOException {
        try (InputStream in = new FileInputStream(f)) {
            ByteArrayOutputStream bo = new ByteArrayOutputStream(Math.min(max, 64 * 1024));
            byte[] buf = new byte[16 * 1024];
            int n, total = 0;
            while (total < max && (n = in.read(buf, 0, Math.min(buf.length, max - total))) > 0) {
                bo.write(buf, 0, n);
                total += n;
            }
            return bo.toByteArray();
        }
    }

    static boolean validUtf8(byte[] b) {
        try {
            StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(b));
            return true;
        } catch (CharacterCodingException e) {
            return false;
        }
    }

    public static String decodeText(byte[] b) {
        if (b.length >= 3 && (b[0] & 0xFF) == 0xEF && (b[1] & 0xFF) == 0xBB && (b[2] & 0xFF) == 0xBF) {
            return new String(b, 3, b.length - 3, StandardCharsets.UTF_8);
        }
        if (b.length >= 2 && (((b[0] & 0xFF) == 0xFF && (b[1] & 0xFF) == 0xFE) || ((b[0] & 0xFF) == 0xFE && (b[1] & 0xFF) == 0xFF))) {
            Charset cs = (b[0] & 0xFF) == 0xFF ? StandardCharsets.UTF_16LE : StandardCharsets.UTF_16BE;
            return new String(b, 2, b.length - 2, cs);
        }
        int probe = Math.min(b.length, 4096);
        for (int i = 0; i < probe; i++) if (b[i] == 0) return ""; // бинарный
        if (validUtf8(b) || (b.length > 4 && validUtf8(java.util.Arrays.copyOf(b, b.length - 3)))) return new String(b, StandardCharsets.UTF_8);
        try {
            return new String(b, Charset.forName("windows-1251"));
        } catch (RuntimeException e) {
            return new String(b, StandardCharsets.ISO_8859_1);
        }
    }

    public static String extractText(File f, String ext, int maxChars) {
        try {
            String s;
            if (Util.ZIP_DOC_EXTS.contains(ext)) {
                s = zipDoc(f, ext, maxChars);
            } else {
                int n = MAX_TEXT_READ;
                if (maxChars > 0 && maxChars * 4 < n) n = maxChars * 4;
                s = decodeText(readHead(f, n));
                if (ext.equals("html") || ext.equals("htm") || ext.equals("xml") || ext.equals("fb2") || ext.equals("svg")) {
                    s = unescape(stripXml(s, true));
                }
            }
            if (maxChars > 0 && s.length() > maxChars) s = s.substring(0, maxChars);
            return s;
        } catch (Throwable t) {
            return "";
        }
    }

    // ---------- XML ----------
    private static boolean officeSep(String tag) {
        switch (tag) {
            case "w:p": case "a:p": case "text:p": case "text:h": case "si": case "row":
            case "w:br": case "w:tab": case "w:tc": case "table:table-cell": case "text:line-break":
            case "text:tab": case "a:br": case "text:s": case "w:cr":
                return true;
        }
        return false;
    }

    static String stripXml(String s, boolean generic) {
        StringBuilder sb = new StringBuilder(s.length() / 2);
        int n = s.length();
        String skipUntil = null;
        int i = 0;
        while (i < n) {
            char c = s.charAt(i);
            if (c != '<') {
                int j = s.indexOf('<', i);
                if (j < 0) j = n;
                if (skipUntil == null) sb.append(s, i, j);
                i = j;
                continue;
            }
            int j = s.indexOf('>', i);
            if (j < 0) break;
            String tag = s.substring(i + 1, j);
            i = j + 1;
            if (tag.isEmpty()) continue;
            boolean closing = tag.charAt(0) == '/';
            String name = closing ? tag.substring(1) : tag;
            int k = 0;
            while (k < name.length() && " \t\r\n/".indexOf(name.charAt(k)) < 0) k++;
            name = name.substring(0, k).toLowerCase(Locale.ROOT);
            boolean selfClose = tag.charAt(tag.length() - 1) == '/';
            if (generic) {
                if (skipUntil != null) {
                    if (closing && name.equals(skipUntil)) skipUntil = null;
                    continue;
                }
                if (!closing && (name.equals("script") || name.equals("style")) && !selfClose) {
                    skipUntil = name;
                    continue;
                }
                sb.append(' ');
                continue;
            }
            if (officeSep(name) && (closing || selfClose || name.equals("w:br") || name.equals("w:tab"))) sb.append(' ');
        }
        return sb.toString();
    }

    static String unescape(String s) {
        if (s.indexOf('&') < 0) return s;
        return s.replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", "\"")
                .replace("&apos;", "'").replace("&nbsp;", " ").replace("&#39;", "'").replace("&amp;", "&");
    }

    private static int num(String s) {
        int st = -1, en = -1;
        for (int i = 0; i < s.length(); i++) {
            if (Character.isDigit(s.charAt(i))) { if (st < 0) st = i; en = i + 1; } else if (st >= 0) break;
        }
        if (st < 0) return 0;
        try { return Integer.parseInt(s.substring(st, en)); } catch (NumberFormatException e) { return 0; }
    }

    static String zipDoc(File f, String ext, int maxChars) throws IOException {
        try (ZipFile zf = new ZipFile(f)) {
            List<ZipEntry> parts = new ArrayList<>();
            java.util.Enumeration<? extends ZipEntry> en = zf.entries();
            while (en.hasMoreElements()) {
                ZipEntry e = en.nextElement();
                String n = e.getName();
                boolean pick;
                switch (ext) {
                    case "docx": pick = n.equals("word/document.xml") || n.startsWith("word/header") || n.startsWith("word/footnotes"); break;
                    case "xlsx": pick = n.equals("xl/sharedStrings.xml"); break;
                    case "pptx": pick = n.startsWith("ppt/slides/slide") && n.endsWith(".xml"); break;
                    default: pick = n.equals("content.xml");
                }
                if (pick) parts.add(e);
            }
            Collections.sort(parts, (a, b) -> {
                int na = num(a.getName()), nb = num(b.getName());
                if (na != nb) return Integer.compare(na, nb);
                return a.getName().compareTo(b.getName());
            });
            StringBuilder sb = new StringBuilder();
            for (ZipEntry e : parts) {
                try (InputStream in = zf.getInputStream(e)) {
                    ByteArrayOutputStream bo = new ByteArrayOutputStream();
                    byte[] buf = new byte[16 * 1024];
                    int n, total = 0;
                    while (total < MAX_ZIP_PART && (n = in.read(buf)) > 0) { bo.write(buf, 0, n); total += n; }
                    sb.append(unescape(stripXml(new String(bo.toByteArray(), StandardCharsets.UTF_8), false))).append('\n');
                }
                if (maxChars > 0 && sb.length() > maxChars) break;
            }
            return sb.toString();
        }
    }

    // ---------- сниппеты ----------
    public static String snippetAround(String text, List<String> toks, int width) {
        String low = Util.norm(text);
        int best = -1;
        for (String t : toks) {
            if (t.isEmpty()) continue;
            int i = low.indexOf(t);
            if (i >= 0 && (best < 0 || i < best)) best = i;
        }
        if (best < 0) return "";
        int start = Math.max(0, best - width / 3);
        int end = Math.min(text.length(), start + width);
        if (start > text.length()) start = text.length();
        String seg = text.substring(start, end).trim().replaceAll("\\s+", " ");
        if (start > 0) seg = "…" + seg;
        if (end < text.length()) seg += "…";
        return seg;
    }

    public static String headSnippet(String text, int maxLines, int maxChars) {
        StringBuilder sb = new StringBuilder();
        int lines = 0, total = 0;
        for (String l : text.replace("\r", "").split("\n")) {
            l = l.replaceAll("\\s+$", "");
            if (l.trim().isEmpty()) continue;
            if (l.length() > 90) l = l.substring(0, 90);
            if (lines > 0) sb.append('\n');
            sb.append(l);
            lines++;
            total += l.length();
            if (lines >= maxLines || total > maxChars) break;
        }
        return sb.toString();
    }
}
