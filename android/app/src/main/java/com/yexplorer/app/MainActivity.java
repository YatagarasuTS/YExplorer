package com.yexplorer.app;

import android.Manifest;
import android.app.Activity;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Canvas;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.provider.Settings;
import android.webkit.MimeTypeMap;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Toast;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;

/**
 * Окно приложения: WebView с интерфейсом из assets. Запросы к https://yexplorer.app/api/...
 * перехватываются и обрабатываются здесь, сеть не используется.
 */
public class MainActivity extends Activity {
    static final String HOST = "yexplorer.app";

    private WebView web;
    private Engine engine;
    private final Map<String, byte[]> thumbs = new LinkedHashMap<String, byte[]>(64, 0.75f, true) {
        @Override
        protected boolean removeEldestEntry(Map.Entry<String, byte[]> e) { return size() > 120; }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().setStatusBarColor(0xFF0C0C0E);
        getWindow().setNavigationBarColor(0xFF0C0C0E);
        engine = new Engine(new File(getFilesDir(), "yx"));
        web = new WebView(this);
        web.setBackgroundColor(0xFF0C0C0E);
        setContentView(web);
        WebSettings ws = web.getSettings();
        ws.setJavaScriptEnabled(true);
        ws.setDomStorageEnabled(true);
        ws.setAllowFileAccess(false);
        ws.setAllowContentAccess(false);
        ws.setBuiltInZoomControls(false);
        ws.setSupportZoom(false);
        ws.setTextZoom(100);
        web.setWebViewClient(new WebViewClient() {
            @Override
            public WebResourceResponse shouldInterceptRequest(WebView view, WebResourceRequest request) {
                return intercept(request.getUrl());
            }

            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                return !HOST.equals(request.getUrl().getHost());
            }
        });
        web.loadUrl("https://" + HOST + "/index.html");
        refreshAccess();
    }

    // ---------- доступ к файлам ----------
    private boolean hasAccess() {
        if (Build.VERSION.SDK_INT >= 30) return Environment.isExternalStorageManager();
        return checkSelfPermission(Manifest.permission.READ_EXTERNAL_STORAGE) == PackageManager.PERMISSION_GRANTED;
    }

    private void askAccess() {
        if (Build.VERSION.SDK_INT >= 30) {
            try {
                startActivity(new Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION, Uri.parse("package:" + getPackageName())));
            } catch (Exception e) {
                try { startActivity(new Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION)); } catch (Exception ignored) { }
            }
        } else {
            requestPermissions(new String[]{Manifest.permission.READ_EXTERNAL_STORAGE, Manifest.permission.WRITE_EXTERNAL_STORAGE}, 1);
        }
    }

    private List<File> storageRoots() {
        List<File> roots = new ArrayList<>();
        File prim = Environment.getExternalStorageDirectory();
        roots.add(prim);
        File[] dirs = getExternalFilesDirs(null);
        if (dirs != null) {
            for (File d : dirs) {
                if (d == null) continue;
                String p = d.getAbsolutePath();
                int k = p.indexOf("/Android/");
                if (k > 0) {
                    File r = new File(p.substring(0, k));
                    if (!r.equals(prim) && r.isDirectory()) roots.add(r);
                }
            }
        }
        return roots;
    }

    private void refreshAccess() {
        if (hasAccess()) {
            if (!engine.isStarted()) {
                engine.setRoots(storageRoots());
                engine.start();
            }
        } else {
            engine.setStage("noperm");
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        refreshAccess();
    }

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        refreshAccess();
    }

    @Override
    public void onBackPressed() {
        if (web != null && web.canGoBack()) web.goBack(); else super.onBackPressed();
    }

    // ---------- перехват запросов WebView ----------
    private static WebResourceResponse response(int code, String mime, byte[] body, boolean cache) {
        Map<String, String> h = new HashMap<>();
        h.put("Cache-Control", cache ? "private, max-age=3600" : "no-store");
        String reason = code == 200 ? "OK" : (code == 404 ? "Not Found" : "Error");
        return new WebResourceResponse(mime, "utf-8", code, reason, h, new ByteArrayInputStream(body));
    }

    private static WebResourceResponse json(Engine.Resp r) {
        return response(r.code, "application/json", r.json.getBytes(StandardCharsets.UTF_8), false);
    }

    private WebResourceResponse intercept(Uri uri) {
        if (!HOST.equals(uri.getHost())) return response(404, "text/plain", new byte[0], false); // внешняя сеть закрыта
        String path = uri.getPath();
        if (path == null || path.equals("/") || path.isEmpty()) path = "/index.html";
        if (path.startsWith("/api/")) {
            Map<String, String> q = new HashMap<>();
            for (String n : uri.getQueryParameterNames()) q.put(n, uri.getQueryParameter(n));
            try {
                switch (path) {
                    case "/api/thumb": return thumb(q.get("p"), q.get("w"));
                    case "/api/open": return open(q.get("p"), false);
                    case "/api/reveal": return open(q.get("p"), true);
                    case "/api/grant":
                        runOnUiThread(this::askAccess);
                        return json(new Engine.Resp(200, "{\"ok\":true}"));
                    default: return json(engine.handle(path, q));
                }
            } catch (Throwable t) {
                return json(new Engine.Resp(500, "{\"error\":\"" + String.valueOf(t.getMessage()).replace("\"", "'") + "\"}"));
            }
        }
        try {
            InputStream in = getAssets().open(path.substring(1));
            ByteArrayOutputStream bo = new ByteArrayOutputStream();
            byte[] buf = new byte[16 * 1024];
            int n;
            while ((n = in.read(buf)) > 0) bo.write(buf, 0, n);
            in.close();
            return response(200, mimeOf(path), bo.toByteArray(), false);
        } catch (IOException e) {
            return response(404, "text/plain", new byte[0], false);
        }
    }

    private static String mimeOf(String path) {
        String p = path.toLowerCase(Locale.ROOT);
        if (p.endsWith(".html")) return "text/html";
        if (p.endsWith(".png")) return "image/png";
        if (p.endsWith(".js")) return "application/javascript";
        if (p.endsWith(".css")) return "text/css";
        if (p.endsWith(".json")) return "application/json";
        return "application/octet-stream";
    }

    // ---------- миниатюры ----------
    private WebResourceResponse thumb(String p, String ws) {
        File f = engine.fileFor(p);
        if (f == null || !f.isFile()) return response(404, "text/plain", new byte[0], false);
        int w = 360;
        try { w = Math.max(64, Math.min(1600, Integer.parseInt(ws))); } catch (Exception ignored) { }
        String key = f.getAbsolutePath() + "|" + f.lastModified() + "|" + w;
        byte[] cached;
        synchronized (thumbs) { cached = thumbs.get(key); }
        if (cached == null) {
            BitmapFactory.Options o = new BitmapFactory.Options();
            o.inJustDecodeBounds = true;
            BitmapFactory.decodeFile(f.getAbsolutePath(), o);
            if (o.outWidth <= 0 || o.outHeight <= 0) return response(404, "text/plain", new byte[0], false);
            int sample = 1;
            while (o.outWidth / (sample * 2) >= w) sample *= 2;
            BitmapFactory.Options o2 = new BitmapFactory.Options();
            o2.inSampleSize = sample;
            Bitmap src = BitmapFactory.decodeFile(f.getAbsolutePath(), o2);
            if (src == null) return response(404, "text/plain", new byte[0], false);
            int tw = Math.min(w, src.getWidth());
            int th = Math.max(1, src.getHeight() * tw / src.getWidth());
            Bitmap scaled = tw == src.getWidth() ? src : Bitmap.createScaledBitmap(src, tw, th, true);
            Bitmap flat = Bitmap.createBitmap(scaled.getWidth(), scaled.getHeight(), Bitmap.Config.ARGB_8888);
            Canvas c = new Canvas(flat);
            c.drawColor(Color.WHITE);
            c.drawBitmap(scaled, 0, 0, null);
            ByteArrayOutputStream bo = new ByteArrayOutputStream();
            flat.compress(Bitmap.CompressFormat.JPEG, 80, bo);
            cached = bo.toByteArray();
            synchronized (thumbs) { thumbs.put(key, cached); }
        }
        return response(200, "image/jpeg", cached, true);
    }

    // ---------- открыть файл / показать в папке ----------
    private WebResourceResponse open(String p, final boolean reveal) {
        final File f = engine.fileFor(p);
        if (f == null) return json(new Engine.Resp(400, "{\"error\":\"файл не найден\"}"));
        runOnUiThread(() -> {
            try {
                if (reveal || f.isDirectory()) revealFolder(f); else openFile(f);
            } catch (ActivityNotFoundException e) {
                Toast.makeText(this, "Нет приложения для этого типа файлов", Toast.LENGTH_LONG).show();
            } catch (Exception e) {
                Toast.makeText(this, f.getAbsolutePath(), Toast.LENGTH_LONG).show();
            }
        });
        return json(new Engine.Resp(200, "{\"ok\":true}"));
    }

    private void openFile(File f) {
        String name = f.getName().toLowerCase(Locale.ROOT);
        int k = name.lastIndexOf('.');
        String mime = null;
        if (k >= 0) mime = MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substring(k + 1));
        if (mime == null) mime = "*/*";
        Intent i = new Intent(Intent.ACTION_VIEW);
        i.setDataAndType(FilesProvider.uriFor(f), mime);
        i.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION | Intent.FLAG_ACTIVITY_NEW_TASK);
        startActivity(i);
    }

    private void revealFolder(File f) {
        File dir = f.isDirectory() ? f : f.getParentFile();
        String prim = Environment.getExternalStorageDirectory().getAbsolutePath();
        String d = dir == null ? prim : dir.getAbsolutePath();
        if (!d.startsWith(prim)) {
            Toast.makeText(this, d, Toast.LENGTH_LONG).show();
            return;
        }
        String rel = d.length() > prim.length() ? d.substring(prim.length() + 1) : "";
        Uri u = Uri.parse("content://com.android.externalstorage.documents/document/primary%3A" + Uri.encode(rel));
        Intent i = new Intent(Intent.ACTION_VIEW);
        i.setDataAndType(u, "vnd.android.document/directory");
        i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        try {
            startActivity(i);
        } catch (ActivityNotFoundException e) {
            Toast.makeText(this, d, Toast.LENGTH_LONG).show();
        }
    }
}
