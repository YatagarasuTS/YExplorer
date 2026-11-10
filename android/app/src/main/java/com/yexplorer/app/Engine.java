package com.yexplorer.app;

import java.io.File;
import java.io.FileOutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.BitSet;
import java.util.Collections;
import java.util.Comparator;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.locks.ReentrantReadWriteLock;

/** Индекс файлов и поиск: имена, содержимое, опечатки, раскладка. Без зависимостей от Android. */
public final class Engine {
    public static final String VERSION = "1.0.0";

    public static final class E {
        String path, name, low, ext;
        long size, mod;
        byte cat;
        boolean dir;
    }

    static final class Snap {
        final E[] e;
        final HashMap<String, int[]> nameVocab; // слово из имени -> индексы файлов
        final int[] counts = new int[6];
        Snap(E[] e, HashMap<String, int[]> v) { this.e = e; this.nameVocab = v; }
    }

    static final class Hit {
        final int idx; final float score; final int flags;
        Hit(int idx, float score, int flags) { this.idx = idx; this.score = score; this.flags = flags; }
    }

    static final class Result { Snap snap; List<Hit> hits; List<String> toks; }

    static final int F_NAME = 1, F_CONTENT = 2, F_FUZZY = 4;
    static final String[] CAT_NAMES = {"other", "image", "doc", "code", "media", "folder"};

    private final File dataDir;
    private volatile Snap snap = new Snap(new E[0], null);
    private volatile Map<String, Object> cfg;
    private final List<File> roots = new ArrayList<>();

    // содержимое
    private final ReentrantReadWriteLock cl = new ReentrantReadWriteLock();
    private HashMap<String, IntList> post = new HashMap<>();
    private String[] vocab = new String[0];
    private boolean vocabDirty = true;
    private long vocabAt;
    private int docs;

    // состояние
    private volatile String stage = "noperm";
    private final AtomicLong scanned = new AtomicLong();
    private final AtomicLong cDone = new AtomicLong(), cTotal = new AtomicLong();
    private volatile long cStart;
    private volatile boolean firstRun;
    private volatile long scanMs;
    private volatile boolean started;
    private volatile int generation;
    private final Map<String, String> snipCache = Collections.synchronizedMap(new LinkedHashMap<String, String>(256, 0.75f, true) {
        @Override protected boolean removeEldestEntry(Map.Entry<String, String> e) { return size() > 1500; }
    });

    static final class IntList {
        int[] a = new int[4]; int n;
        void add(int v) { if (n == a.length) a = Arrays.copyOf(a, n * 2); a[n++] = v; }
    }

    public Engine(File dataDir) {
        this.dataDir = dataDir;
        dataDir.mkdirs();
        firstRun = !new File(dataDir, "indexed.flag").exists();
        loadConfig();
    }

    // ---------------- настройки ----------------
    static final String[] DEFAULT_EXCL = {".thumbnails", ".cache", ".trash", ".Trash", "cache", "code_cache", "LOST.DIR", "node_modules", ".git", "Android/data", "Android/obb", ".nomedia_dir"};

    private Map<String, Object> defaultConfig() {
        Map<String, Object> c = new LinkedHashMap<>();
        c.put("theme", "dark");
        Map<String, Object> cu = new LinkedHashMap<>();
        cu.put("bg", "#12101a"); cu.put("panel", "#1c1830"); cu.put("text", "#efeaff"); cu.put("accent", "#9b6dff");
        c.put("custom", cu);
        c.put("folders", new ArrayList<Object>());
        c.put("exclusions", new ArrayList<Object>(Arrays.asList((Object[]) DEFAULT_EXCL)));
        c.put("maxResults", 60.0);
        c.put("startAtLogin", Boolean.FALSE);
        c.put("content", Boolean.TRUE);
        c.put("ocr", Boolean.FALSE);
        c.put("typos", Boolean.TRUE);
        c.put("view", "grid");
        return c;
    }

    private void loadConfig() {
        Map<String, Object> c = defaultConfig();
        try {
            File f = new File(dataDir, "settings.json");
            if (f.exists()) {
                Map<String, Object> saved = Json.parseObject(new String(Extract.readHead(f, 1 << 20), StandardCharsets.UTF_8));
                if (saved != null) merge(c, saved);
            }
        } catch (Exception ignored) { }
        cfg = c;
    }

    @SuppressWarnings("unchecked")
    private static void merge(Map<String, Object> dst, Map<String, Object> src) {
        for (Map.Entry<String, Object> e : src.entrySet()) {
            if (!dst.containsKey(e.getKey())) continue;
            Object cur = dst.get(e.getKey()), v = e.getValue();
            if (cur instanceof Map && v instanceof Map) merge((Map<String, Object>) cur, (Map<String, Object>) v);
            else if (cur instanceof List && v instanceof List) dst.put(e.getKey(), v);
            else if (cur instanceof String && v instanceof String) dst.put(e.getKey(), v);
            else if (cur instanceof Boolean && v instanceof Boolean) dst.put(e.getKey(), v);
            else if (cur instanceof Double && v instanceof Number) dst.put(e.getKey(), ((Number) v).doubleValue());
        }
    }

    private boolean cfgBool(String k) { return Boolean.TRUE.equals(cfg.get(k)); }

    private void saveConfig() {
        try (FileOutputStream o = new FileOutputStream(new File(dataDir, "settings.json"))) {
            o.write(Json.write(cfg).getBytes(StandardCharsets.UTF_8));
        } catch (Exception ignored) { }
    }

    @SuppressWarnings("unchecked")
    private List<String> cfgList(String k) {
        List<String> out = new ArrayList<>();
        Object o = cfg.get(k);
        if (o instanceof List) for (Object x : (List<Object>) o) if (x instanceof String && !((String) x).trim().isEmpty()) out.add(((String) x).trim());
        return out;
    }

    private void sanitize() {
        String t = String.valueOf(cfg.get("theme"));
        if (!t.equals("dark") && !t.equals("light") && !t.equals("custom")) cfg.put("theme", "dark");
        double mr = ((Number) cfg.get("maxResults")).doubleValue();
        cfg.put("maxResults", (double) Math.max(10, Math.min(500, (int) mr)));
        if (!"list".equals(cfg.get("view"))) cfg.put("view", "grid");
        cfg.put("ocr", Boolean.FALSE);
        cfg.put("startAtLogin", Boolean.FALSE);
    }

    // ---------------- запуск индексации ----------------
    public synchronized void setRoots(List<File> r) {
        roots.clear();
        roots.addAll(r);
    }

    public boolean isStarted() { return started; }

    public synchronized void start() {
        if (started) return;
        started = true;
        stage = "scanning";
        runAsync();
    }

    public synchronized void reindex() {
        if (!started) return;
        generation++;
        runAsync();
    }

    private void runAsync() {
        final int gen = generation;
        Thread t = new Thread(() -> runOnce(gen), "yx-index");
        t.setPriority(Thread.NORM_PRIORITY - 1);
        t.setDaemon(true);
        t.start();
    }

    private boolean cancelled(int gen) { return gen != generation; }

    private void runOnce(int gen) {
        try {
            long t0 = System.currentTimeMillis();
            stage = "scanning";
            scanned.set(0);
            Snap s = scan(gen);
            if (s == null || cancelled(gen)) return;
            scanMs = System.currentTimeMillis() - t0;
            cl.writeLock().lock();
            try { post = new HashMap<>(); vocab = new String[0]; vocabDirty = true; docs = 0; } finally { cl.writeLock().unlock(); }
            snap = s;
            if (cfgBool("content")) {
                stage = "content";
                contentStage(s, gen);
                if (cancelled(gen)) return;
            }
            try { new File(dataDir, "indexed.flag").createNewFile(); } catch (Exception ignored) { }
            stage = "ready";
        } catch (Throwable t) {
            stage = "ready";
        }
    }

    // ---------------- сканирование ----------------
    private Snap scan(int gen) {
        List<String> excl = new ArrayList<>();
        for (String x : cfgList("exclusions")) excl.add(x.toLowerCase(Locale.ROOT));
        List<String> rootsToScan = cfgList("folders");
        List<File> rs = new ArrayList<>();
        if (rootsToScan.isEmpty()) rs.addAll(roots); else for (String p : rootsToScan) rs.add(new File(p));
        ArrayList<E> out = new ArrayList<>(1 << 16);
        for (File root : rs) {
            if (!root.isDirectory()) continue;
            ArrayDeque<File> stack = new ArrayDeque<>();
            stack.push(root);
            while (!stack.isEmpty()) {
                if (cancelled(gen)) return null;
                File dir = stack.pop();
                File[] kids = dir.listFiles();
                if (kids == null) continue;
                for (File f : kids) {
                    String name = f.getName();
                    boolean isDir = f.isDirectory();
                    if (isDir) {
                        if (excluded(name, f.getAbsolutePath(), excl)) continue;
                        try { if (Files.isSymbolicLink(f.toPath())) continue; } catch (Throwable ignored) { }
                    } else if (name.startsWith("~$") || name.equals(".nomedia") || name.equals("desktop.ini") || name.equals("Thumbs.db")) {
                        continue;
                    }
                    E e = new E();
                    e.path = f.getAbsolutePath();
                    e.name = name;
                    e.low = Util.norm(name);
                    e.dir = isDir;
                    e.size = isDir ? 0 : f.length();
                    e.mod = f.lastModified() / 1000;
                    if (!isDir) {
                        int k = e.low.lastIndexOf('.');
                        e.ext = (k >= 0 && k < e.low.length() - 1) ? e.low.substring(k + 1) : "";
                    } else e.ext = "";
                    e.cat = Util.catFor(e.ext, isDir);
                    out.add(e);
                    long c = scanned.incrementAndGet();
                    if (isDir) stack.push(f);
                    if (c % 4000 == 0) {
                        Snap part = new Snap(out.toArray(new E[0]), null);
                        if (snap.e.length < part.e.length) snap = part;
                    }
                }
            }
        }
        E[] arr = out.toArray(new E[0]);
        HashMap<String, IntList> tmp = new HashMap<>(arr.length / 2 + 16);
        Snap tmpSnap;
        for (int i = 0; i < arr.length; i++) {
            for (String w : Util.splitWords(arr[i].low)) {
                IntList l = tmp.get(w);
                if (l == null) { l = new IntList(); tmp.put(w, l); }
                if (l.n == 0 || l.a[l.n - 1] != i) l.add(i);
            }
        }
        HashMap<String, int[]> vocabMap = new HashMap<>(tmp.size() * 2);
        for (Map.Entry<String, IntList> en : tmp.entrySet()) vocabMap.put(en.getKey(), Arrays.copyOf(en.getValue().a, en.getValue().n));
        tmpSnap = new Snap(arr, vocabMap);
        for (E e : arr) tmpSnap.counts[e.cat]++;
        return tmpSnap;
    }

    private static boolean excluded(String name, String path, List<String> excl) {
        String ln = name.toLowerCase(Locale.ROOT);
        String lp = path.toLowerCase(Locale.ROOT);
        for (String x : excl) {
            if (x.indexOf('/') >= 0) {
                if (lp.endsWith("/" + x) || lp.equals(x)) return true;
            } else if (ln.equals(x)) return true;
        }
        return false;
    }

    // ---------------- содержимое ----------------
    static boolean eligible(E e) {
        if (e.dir) return false;
        if (Util.TEXT_EXTS.contains(e.ext)) return e.size <= 64L << 20;
        if (Util.ZIP_DOC_EXTS.contains(e.ext)) return e.size <= 40L << 20;
        return false;
    }

    private void contentStage(Snap s, int gen) {
        List<Integer> todo = new ArrayList<>();
        for (int i = 0; i < s.e.length; i++) if (eligible(s.e[i])) todo.add(i);
        Collections.sort(todo, (a, b) -> Long.compare(s.e[b].mod, s.e[a].mod));
        cTotal.set(todo.size());
        cDone.set(0);
        cStart = System.currentTimeMillis();
        int workers = Math.max(2, Math.min(4, Runtime.getRuntime().availableProcessors() - 1));
        ExecutorService ex = Executors.newFixedThreadPool(workers, r -> {
            Thread t = new Thread(r, "yx-content");
            t.setDaemon(true);
            t.setPriority(Thread.MIN_PRIORITY + 1);
            return t;
        });
        final AtomicInteger next = new AtomicInteger();
        for (int w = 0; w < workers; w++) {
            ex.execute(() -> {
                HashMap<String, Boolean> seen = new HashMap<>();
                while (!cancelled(gen)) {
                    int k = next.getAndIncrement();
                    if (k >= todo.size()) break;
                    int idx = todo.get(k);
                    E e = s.e[idx];
                    String text = Extract.extractText(new File(e.path), e.ext, 0);
                    seen.clear();
                    final List<String> words = new ArrayList<>();
                    Util.tokenize(text, wd -> {
                        if (seen.size() < 3000 && seen.put(wd, Boolean.TRUE) == null) words.add(wd);
                    });
                    cl.writeLock().lock();
                    try {
                        for (String wd : words) {
                            IntList l = post.get(wd);
                            if (l == null) { l = new IntList(); post.put(wd, l); }
                            l.add(idx);
                        }
                        docs++;
                        vocabDirty = true;
                    } finally { cl.writeLock().unlock(); }
                    cDone.incrementAndGet();
                }
            });
        }
        ex.shutdown();
        try { ex.awaitTermination(6, TimeUnit.HOURS); } catch (InterruptedException ignored) { }
    }

    private void refreshVocab() {
        cl.readLock().lock();
        boolean need;
        try { need = vocabDirty && (vocab.length == 0 || System.currentTimeMillis() - vocabAt > 2000); } finally { cl.readLock().unlock(); }
        if (!need) return;
        cl.writeLock().lock();
        try {
            if (vocabDirty) {
                String[] v = post.keySet().toArray(new String[0]);
                Arrays.sort(v);
                vocab = v;
                vocabDirty = false;
                vocabAt = System.currentTimeMillis();
            }
        } finally { cl.writeLock().unlock(); }
    }

    private List<String> expandWord(String sub, boolean fuzzy) {
        String[] v = vocab;
        List<String> out = new ArrayList<>();
        if (!fuzzy) {
            prefixWords(v, sub, out);
            if (out.isEmpty()) {
                int cut = sub.length() >= 7 ? 2 : (sub.length() >= 5 ? 1 : 0);
                if (cut > 0) prefixWords(v, sub.substring(0, sub.length() - cut), out);
            }
            return out;
        }
        int d = Util.maxTypos(sub.length());
        if (d == 0) return out;
        for (String w : v) {
            int lw = w.length();
            if (lw < sub.length() - 2 * d || lw > sub.length() + 3 * d + 2) continue;
            if (Util.osa(sub, w, d) <= d) {
                out.add(w);
                if (out.size() >= 100) break;
            }
        }
        return out;
    }

    private static void prefixWords(String[] v, String p, List<String> out) {
        int lo = 0, hi = v.length;
        while (lo < hi) {
            int mid = (lo + hi) >>> 1;
            if (v[mid].compareTo(p) < 0) lo = mid + 1; else hi = mid;
        }
        for (int k = lo; k < v.length && v[k].startsWith(p) && out.size() < 400; k++) out.add(v[k]);
    }

    /** для каждого файла — маска токенов, найденных в содержимом */
    private byte[] contentMask(List<String> toks, int n, boolean fuzzy, int only) {
        refreshVocab();
        cl.readLock().lock();
        try {
            if (post.isEmpty()) return null;
            byte[] mask = null;
            for (int ti = 0; ti < toks.size(); ti++) {
                if (only >= 0 && only != ti) continue;
                final List<String> subs = new ArrayList<>();
                Util.tokenize(toks.get(ti), subs::add);
                if (subs.isEmpty()) continue;
                BitSet acc = null;
                for (String sub : subs) {
                    BitSet b = new BitSet(n);
                    for (String w : expandWord(sub, fuzzy)) {
                        IntList l = post.get(w);
                        if (l == null) continue;
                        for (int k = 0; k < l.n; k++) if (l.a[k] < n) b.set(l.a[k]);
                    }
                    if (acc == null) acc = b; else acc.and(b);
                }
                if (mask == null) mask = new byte[n];
                for (int i = acc.nextSetBit(0); i >= 0; i = acc.nextSetBit(i + 1)) mask[i] |= (byte) (1 << ti);
            }
            return mask;
        } finally { cl.readLock().unlock(); }
    }

    // ---------------- поиск ----------------
    static List<String> queryTokens(String raw) {
        List<String> out = new ArrayList<>();
        for (String f : Util.norm(raw.trim()).split("\\s+")) {
            if (f.isEmpty()) continue;
            if (out.size() >= 6) break;
            out.add(f);
        }
        return out;
    }

    static boolean sepByte(char c) {
        return c == ' ' || c == '.' || c == '_' || c == '-' || c == '(' || c == ')' || c == '[' || c == ']' || c == '+' || c == ',' || c == '\'';
    }

    static float tokenScore(String low, String t) {
        int i = low.indexOf(t);
        if (i < 0) return 0;
        float s;
        if (low.length() == t.length()) s = 1000;
        else if (i == 0 && low.charAt(t.length()) == '.') s = 950;
        else if (i == 0) s = 800;
        else if (sepByte(low.charAt(i - 1))) s = 600;
        else s = 300;
        return s + 100f * t.length() / low.length();
    }

    private static String parentOf(String p) {
        int i = p.lastIndexOf('/');
        return i > 0 ? p.substring(0, i) : "";
    }

    Result search(String raw, boolean content, boolean typos) {
        Snap s = snap;
        E[] es = s.e;
        int n = es.length;
        Result res = new Result();
        res.snap = s;
        List<String> toks = queryTokens(raw);
        res.toks = toks;
        List<Hit> hits = new ArrayList<>();
        res.hits = hits;
        if (toks.isEmpty()) {
            for (int i = 0; i < n; i++) hits.add(new Hit(i, (float) es[i].mod, F_NAME));
            return res;
        }
        int nt = toks.size();
        int full = (1 << nt) - 1;
        byte[] nm = new byte[n];
        float[] ns = new float[n];
        for (int i = 0; i < n; i++) {
            String low = es[i].low;
            int m = 0; float sc = 0;
            for (int ti = 0; ti < nt; ti++) {
                float v = tokenScore(low, toks.get(ti));
                if (v > 0) { m |= 1 << ti; sc += v; }
            }
            nm[i] = (byte) m; ns[i] = sc;
        }
        if (nt > 1) {
            for (int i = 0; i < n; i++) {
                int m = nm[i] & 0xFF;
                if (m == 0 || m == full) continue;
                String dir = Util.norm(parentOf(es[i].path));
                for (int ti = 0; ti < nt; ti++) {
                    if ((m & (1 << ti)) == 0 && dir.contains(toks.get(ti))) { nm[i] |= (byte) (1 << ti); ns[i] += 150; }
                }
            }
        }
        byte[] cm = content ? contentMask(toks, n, false, -1) : null;
        byte[] fz = new byte[n];
        if (typos) {
            for (int ti = 0; ti < nt; ti++) {
                String t = toks.get(ti);
                if (t.length() < 3) continue;
                int bit = 1 << ti, hitCount = 0;
                for (int i = 0; i < n && hitCount == 0; i++) {
                    if ((nm[i] & bit) != 0 || (cm != null && (cm[i] & bit) != 0)) hitCount++;
                }
                if (hitCount > 0) continue;
                Map<Integer, Float> found = fuzzyName(s, t);
                for (Map.Entry<Integer, Float> en : found.entrySet()) { fz[en.getKey()] |= (byte) bit; ns[en.getKey()] += en.getValue(); }
                if (content) {
                    byte[] c2 = contentMask(toks, n, true, ti);
                    if (c2 != null) {
                        if (cm == null) cm = new byte[n];
                        for (int i = 0; i < n; i++) if ((c2[i] & bit) != 0) cm[i] |= (byte) bit;
                    }
                }
            }
        }
        long now = System.currentTimeMillis() / 1000;
        for (int i = 0; i < n; i++) {
            int a = nm[i] & 0xFF, c = cm == null ? 0 : cm[i] & 0xFF, f = fz[i] & 0xFF;
            if ((a | c | f) != full) continue;
            E e = es[i];
            float score = ns[i];
            int only = c & ~a & ~f;
            score += 180f * Integer.bitCount(only);
            int fl = 0;
            if (a != 0) fl |= F_NAME;
            if (c != 0) { fl |= F_CONTENT; if (a != 0) score += 60; }
            if (f != 0) fl |= F_FUZZY;
            score /= nt;
            if (e.mod > 0) {
                double age = Math.max(0, (now - e.mod) / 86400.0);
                score += (float) (40 * Math.exp(-age / 60));
            }
            if (e.dir) score += 10;
            hits.add(new Hit(i, score, fl));
        }
        return res;
    }

    private Map<Integer, Float> fuzzyName(Snap s, String t) {
        Map<Integer, Float> out = new HashMap<>();
        int d = Util.maxTypos(t.length());
        if (d == 0 || s.nameVocab == null) return out;
        for (Map.Entry<String, int[]> en : s.nameVocab.entrySet()) {
            String w = en.getKey();
            int lw = w.length();
            if (lw < t.length() - d || lw > t.length() + 6) continue;
            int best = Util.osa(t, w, d);
            if (lw > t.length()) {
                for (int L = t.length() - 1; L <= t.length() + 1; L++) {
                    if (L > 1 && L <= lw) best = Math.min(best, Util.osa(t, w.substring(0, L), d));
                }
            }
            if (best <= d) {
                float sc = 230 - 50 * best;
                for (int idx : en.getValue()) {
                    Float cur = out.get(idx);
                    if (cur == null || cur < sc) out.put(idx, sc);
                }
            }
        }
        return out;
    }

    // ---------------- API ----------------
    public static final class Resp {
        public final int code; public final String json;
        Resp(int code, String json) { this.code = code; this.json = json; }
    }

    private static Resp ok(Object o) { return new Resp(200, Json.write(o)); }
    private static Resp err(int code, String m) { Map<String, Object> e = new LinkedHashMap<>(); e.put("error", m); return new Resp(code, Json.write(e)); }

    public boolean validPath(String p) {
        if (p == null || p.isEmpty()) return false;
        try {
            String c = new File(p).getCanonicalPath();
            List<File> all = new ArrayList<>(roots);
            for (String f : cfgList("folders")) all.add(new File(f));
            for (File r : all) {
                String rc = r.getCanonicalPath();
                if (c.equals(rc) || c.startsWith(rc.endsWith("/") ? rc : rc + "/")) return new File(c).exists();
            }
        } catch (Exception ignored) { }
        return false;
    }

    public File fileFor(String p) { return validPath(p) ? new File(p) : null; }

    /** path: "/api/..."; q — параметры запроса */
    public Resp handle(String path, Map<String, String> q) {
        switch (path) {
            case "/api/status": return ok(status());
            case "/api/search": return doSearch(q);
            case "/api/preview": return doPreview(q.get("p"));
            case "/api/settings": return doSettings(q.get("j"));
            case "/api/reindex": reindex(); return ok(mapOf("ok", Boolean.TRUE));
            default: return err(404, "not found");
        }
    }

    static Map<String, Object> mapOf(Object... kv) {
        Map<String, Object> m = new LinkedHashMap<>();
        for (int i = 0; i + 1 < kv.length; i += 2) m.put(String.valueOf(kv[i]), kv[i + 1]);
        return m;
    }

    public void setStage(String s) { if (!started) stage = s; }

    Map<String, Object> status() {
        Snap s = snap;
        int folders = s.counts[Util.FOLDER];
        long files = s.nameVocab == null ? s.e.length : s.e.length - folders;
        long el = System.currentTimeMillis() - cStart;
        long eta = 0;
        if ("content".equals(stage) && cDone.get() > 20 && el > 500) {
            double rate = cDone.get() / (el / 1000.0);
            eta = (long) ((cTotal.get() - cDone.get()) / rate);
        }
        return mapOf("stage", stage, "scanned", scanned.get(), "files", files, "folders", folders,
                "contentDone", cDone.get(), "contentTotal", cTotal.get(), "ocrDone", 0, "ocrTotal", 0,
                "eta", eta, "firstRun", firstRun, "scanMs", scanMs, "docs", docs, "ocrAvailable", Boolean.FALSE,
                "lastRun", 0);
    }

    private Resp doSettings(String j) {
        if (j != null) {
            Map<String, Object> in = Json.parseObject(j);
            if (in == null) return err(400, "некорректные настройки");
            Map<String, Object> old = cfg;
            Map<String, Object> n = defaultConfig();
            merge(n, old);
            merge(n, in);
            boolean reidx = !n.get("folders").equals(old.get("folders")) || !n.get("exclusions").equals(old.get("exclusions"))
                    || (Boolean.TRUE.equals(n.get("content")) && !Boolean.TRUE.equals(old.get("content")));
            cfg = n;
            sanitize();
            saveConfig();
            if (reidx) reindex();
            return ok(mapOf("config", cfg, "reindex", reidx));
        }
        List<String> shown = cfgList("folders");
        Map<String, Object> c = new LinkedHashMap<>(cfg);
        if (shown.isEmpty()) { List<String> r = new ArrayList<>(); for (File f : roots) r.add(f.getAbsolutePath()); c.put("folders", r); }
        return ok(mapOf("config", c, "exe", "", "windows", Boolean.FALSE, "android", Boolean.TRUE, "version", VERSION, "dataDir", dataDir.getAbsolutePath()));
    }

    private static boolean hasStrongHit(Result r) {
        for (Hit h : r.hits) if ((h.flags & F_FUZZY) == 0) return true;
        return false;
    }

    private Resp doSearch(Map<String, String> q) {
        String raw = q.getOrDefault("q", "").trim();
        String cat = q.getOrDefault("cat", "all");
        String sort = q.getOrDefault("sort", "best");
        int limit = ((Number) cfg.get("maxResults")).intValue();
        try { int l = Integer.parseInt(q.get("limit")); if (l > 0 && l <= 500) limit = l; } catch (Exception ignored) { }
        boolean content = cfgBool("content"), typos = cfgBool("typos");
        long t0 = System.nanoTime();
        Result res = search(raw, content, typos);
        String fixed = "";
        if (!raw.isEmpty() && !hasStrongHit(res)) {
            String f = Util.fixLayout(raw);
            if (f != null) {
                Result r2 = search(f, content, typos);
                if (hasStrongHit(r2) || (res.hits.isEmpty() && !r2.hits.isEmpty())) { res = r2; fixed = f; }
            }
        }
        final E[] es = res.snap.e;
        Map<String, Object> counts = new LinkedHashMap<>();
        int[] cnt = new int[8]; // all files folders images docs code media recent
        long cut = System.currentTimeMillis() / 1000 - 14L * 86400;
        List<Hit> filtered = new ArrayList<>(res.hits.size());
        for (Hit h : res.hits) {
            E e = es[h.idx];
            cnt[0]++;
            if (e.dir) cnt[2]++; else cnt[1]++;
            switch (e.cat) { case Util.IMAGE: cnt[3]++; break; case Util.DOC: cnt[4]++; break; case Util.CODE: cnt[5]++; break; case Util.MEDIA: cnt[6]++; break; default: }
            boolean recent = !e.dir && e.mod >= cut;
            if (recent) cnt[7]++;
            boolean okc;
            switch (cat) {
                case "files": okc = !e.dir; break;
                case "folders": okc = e.dir; break;
                case "images": okc = e.cat == Util.IMAGE; break;
                case "docs": okc = e.cat == Util.DOC; break;
                case "code": okc = e.cat == Util.CODE; break;
                case "media": okc = e.cat == Util.MEDIA; break;
                case "recent": okc = recent; break;
                default: okc = true;
            }
            if (okc) filtered.add(h);
        }
        String[] cn = {"all", "files", "folders", "images", "docs", "code", "media", "recent"};
        for (int i = 0; i < cn.length; i++) counts.put(cn[i], cnt[i]);
        Comparator<Hit> cmp;
        switch (sort) {
            case "name": cmp = (a, b) -> es[a.idx].low.compareTo(es[b.idx].low); break;
            case "date": cmp = (a, b) -> Long.compare(es[b.idx].mod, es[a.idx].mod); break;
            case "size": cmp = (a, b) -> Long.compare(es[b.idx].size, es[a.idx].size); break;
            default: cmp = (a, b) -> {
                int c = Float.compare(b.score, a.score);
                return c != 0 ? c : Long.compare(es[b.idx].mod, es[a.idx].mod);
            };
        }
        int total = filtered.size();
        List<Hit> top;
        if (total > limit * 3 && total > 2000) {
            java.util.PriorityQueue<Hit> pq = new java.util.PriorityQueue<>(limit + 1, cmp.reversed());
            for (Hit h : filtered) { pq.add(h); if (pq.size() > limit) pq.poll(); }
            top = new ArrayList<>(pq);
            Collections.sort(top, cmp);
        } else {
            top = filtered;
            Collections.sort(top, cmp);
            if (top.size() > limit) top = new ArrayList<>(top.subList(0, limit));
        }
        double ms = (System.nanoTime() - t0) / 1e6;

        List<Object> items = new ArrayList<>();
        String key = String.join(" ", res.toks);
        for (Hit h : top) {
            E e = es[h.idx];
            String m = "name";
            if ((h.flags & F_FUZZY) != 0) m = "fuzzy";
            else if ((h.flags & F_NAME) == 0 && (h.flags & F_CONTENT) != 0) m = "content";
            else if ((h.flags & F_NAME) != 0 && (h.flags & F_CONTENT) != 0) m = "both";
            String snip = "";
            if (!e.dir && (e.cat == Util.CODE || e.cat == Util.DOC)) snip = snippet(e, res.toks, key, (h.flags & F_CONTENT) != 0);
            Map<String, Object> it = mapOf("path", e.path, "name", e.name, "dir", parentOf(e.path), "ext", e.ext,
                    "cat", CAT_NAMES[e.cat], "size", e.size, "mod", e.mod, "match", m, "isDir", e.dir);
            if (!snip.isEmpty()) it.put("snip", snip);
            items.add(it);
        }
        return ok(mapOf("items", items, "counts", counts, "total", total, "ms", ms, "fixed", fixed, "toks", res.toks, "indexed", es.length));
    }

    private String snippet(E e, List<String> toks, String key, boolean hasContent) {
        String ck = e.path + "|" + e.mod + "|" + key + "|" + hasContent;
        String c = snipCache.get(ck);
        if (c != null) return c;
        String s;
        File f = new File(e.path);
        if (hasContent) {
            String text = Extract.extractText(f, e.ext, 300000);
            s = Extract.snippetAround(text, toks, 170);
            if (s.isEmpty()) {
                List<String> stems = new ArrayList<>();
                for (String t : toks) if (t.length() > 4) stems.add(t.substring(0, t.length() - 2));
                s = Extract.snippetAround(text, stems, 170);
            }
            if (s.isEmpty()) s = Extract.headSnippet(text, 6, 220);
        } else {
            s = (Util.TEXT_EXTS.contains(e.ext) || Util.ZIP_DOC_EXTS.contains(e.ext)) ? Extract.headSnippet(Extract.extractText(f, e.ext, 3000), 8, 260) : "";
        }
        snipCache.put(ck, s);
        return s;
    }

    private Resp doPreview(String p) {
        if (!validPath(p)) return err(404, "нет файла");
        File f = new File(p);
        String name = f.getName().toLowerCase(Locale.ROOT);
        int k = name.lastIndexOf('.');
        String ext = k >= 0 ? name.substring(k + 1) : "";
        Map<String, Object> out = mapOf("size", f.length(), "mod", f.lastModified() / 1000, "isDir", f.isDirectory(), "cat", CAT_NAMES[Util.catFor(ext, f.isDirectory())]);
        if (f.isDirectory()) {
            String[] names = f.list();
            List<String> l = new ArrayList<>();
            if (names != null) { Arrays.sort(names); for (int i = 0; i < names.length && i < 40; i++) l.add(names[i]); }
            out.put("list", l);
        } else if (Util.TEXT_EXTS.contains(ext) || Util.ZIP_DOC_EXTS.contains(ext)) {
            out.put("text", Extract.extractText(f, ext, 24000));
        }
        return ok(out);
    }
}
