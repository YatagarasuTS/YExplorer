package com.yexplorer.app;

import java.util.Arrays;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.ArrayList;
import java.util.Map;
import java.util.Set;

public final class Util {
    private Util() {}

    public static final byte OTHER = 0, IMAGE = 1, DOC = 2, CODE = 3, MEDIA = 4, FOLDER = 5;

    static Set<String> set(String list) { return new HashSet<>(Arrays.asList(list.split("\\s+"))); }

    static final Set<String> IMAGE_EXTS = set("png jpg jpeg gif bmp webp svg ico tif tiff heic heif avif jfif");
    static final Set<String> DOC_EXTS = set("doc docx pdf txt md rtf odt ods odp xls xlsx csv ppt pptx fb2 epub djvu tsv");
    static final Set<String> CODE_EXTS = set("go py pyw js jsx ts tsx mjs cjs vue svelte java kt kts c h cpp hpp cc cs php rb swift dart lua sh bash bat cmd ps1 sql r scala pl rs html htm css scss less json xml yml yaml toml ini cfg conf gradle properties cmake tex ipynb sln csproj asm smali");
    static final Set<String> MEDIA_EXTS = set("mp3 wav flac ogg m4a aac wma opus amr mp4 mkv avi mov wmv webm flv m4v 3gp mpg mpeg");
    public static final Set<String> TEXT_EXTS = set("txt md markdown log csv tsv json xml yml yaml toml ini cfg conf html htm css scss less js jsx ts tsx mjs cjs vue svelte py pyw go rs java kt kts c h cpp hpp cc cs php rb swift dart lua sh bash bat cmd ps1 sql r scala pl tex srt vtt gradle properties cmake ipynb asm smali fb2 svg vcf");
    public static final Set<String> ZIP_DOC_EXTS = set("docx xlsx pptx odt ods odp");

    public static byte catFor(String ext, boolean dir) {
        if (dir) return FOLDER;
        if (IMAGE_EXTS.contains(ext)) return IMAGE;
        if (CODE_EXTS.contains(ext)) return CODE;
        if (DOC_EXTS.contains(ext)) return DOC;
        if (MEDIA_EXTS.contains(ext)) return MEDIA;
        return OTHER;
    }

    /** нижний регистр + ё→е */
    public static String norm(String s) {
        s = s.toLowerCase();
        return s.indexOf('ё') >= 0 ? s.replace('ё', 'е') : s;
    }

    public interface Sink { void accept(String w); }

    /** слова из букв и цифр, 2..32 символа, нижний регистр */
    public static void tokenize(String text, Sink emit) {
        StringBuilder b = new StringBuilder();
        int n = text.length();
        for (int i = 0; i < n; i++) {
            char c = text.charAt(i);
            if (Character.isLetterOrDigit(c)) {
                c = Character.toLowerCase(c);
                if (c == 'ё') c = 'е';
                b.append(c);
            } else if (b.length() > 0) {
                if (b.length() >= 2 && b.length() <= 32) emit.accept(b.toString());
                b.setLength(0);
            }
        }
        if (b.length() >= 2 && b.length() <= 32) emit.accept(b.toString());
    }

    public static List<String> splitWords(String low) {
        List<String> out = new ArrayList<>(4);
        int st = -1;
        for (int i = 0; i < low.length(); i++) {
            boolean w = Character.isLetterOrDigit(low.charAt(i));
            if (w && st < 0) st = i;
            else if (!w && st >= 0) { out.add(low.substring(st, i)); st = -1; }
        }
        if (st >= 0) out.add(low.substring(st));
        return out;
    }

    // ---- раскладка ----
    private static final String EN = "qwertyuiop[]asdfghjkl;'zxcvbnm,./`";
    private static final String RU = "йцукенгшщзхъфывапролджэячсмитьбю.ё";
    private static final Map<Character, Character> EN2RU = new HashMap<>(), RU2EN = new HashMap<>();
    static {
        for (int i = 0; i < EN.length(); i++) { EN2RU.put(EN.charAt(i), RU.charAt(i)); RU2EN.put(RU.charAt(i), EN.charAt(i)); }
    }

    /** «ghbdtn» → «привет», «руддщ» → «hello»; null если не применимо */
    public static String fixLayout(String q) {
        String low = q.toLowerCase();
        boolean lat = false, cyr = false;
        for (int i = 0; i < low.length(); i++) {
            char c = low.charAt(i);
            if (c >= 'a' && c <= 'z') lat = true;
            else if ((c >= 'а' && c <= 'я') || c == 'ё') cyr = true;
        }
        if (lat == cyr) return null;
        Map<Character, Character> m = cyr ? RU2EN : EN2RU;
        StringBuilder sb = new StringBuilder();
        boolean changed = false;
        for (int i = 0; i < low.length(); i++) {
            Character t = m.get(low.charAt(i));
            if (t != null) { sb.append(t); changed = true; } else sb.append(low.charAt(i));
        }
        return changed ? sb.toString() : null;
    }

    // ---- Дамерау–Левенштейн (OSA) ----
    public static int osa(String a, String b, int limit) {
        int la = a.length(), lb = b.length();
        if (Math.abs(la - lb) > limit) return limit + 1;
        if (la == 0) return lb;
        if (lb == 0) return la;
        int[] p2 = new int[lb + 1], p1 = new int[lb + 1], cur = new int[lb + 1];
        for (int j = 0; j <= lb; j++) p1[j] = j;
        for (int i = 1; i <= la; i++) {
            cur[0] = i;
            int rowMin = cur[0];
            for (int j = 1; j <= lb; j++) {
                int cost = a.charAt(i - 1) == b.charAt(j - 1) ? 0 : 1;
                int v = Math.min(Math.min(p1[j] + 1, cur[j - 1] + 1), p1[j - 1] + cost);
                if (i > 1 && j > 1 && a.charAt(i - 1) == b.charAt(j - 2) && a.charAt(i - 2) == b.charAt(j - 1)) {
                    v = Math.min(v, p2[j - 2] + 1);
                }
                cur[j] = v;
                if (v < rowMin) rowMin = v;
            }
            if (rowMin > limit) return limit + 1;
            int[] t = p2; p2 = p1; p1 = cur; cur = t;
        }
        return p1[lb];
    }

    public static int maxTypos(int n) {
        if (n <= 2) return 0;
        if (n <= 6) return 1;
        return 2;
    }
}
