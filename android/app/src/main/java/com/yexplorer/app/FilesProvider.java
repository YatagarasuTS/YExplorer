package com.yexplorer.app;

import android.content.ContentProvider;
import android.content.ContentValues;
import android.database.Cursor;
import android.database.MatrixCursor;
import android.net.Uri;
import android.os.Environment;
import android.os.ParcelFileDescriptor;
import android.provider.OpenableColumns;
import android.webkit.MimeTypeMap;

import java.io.File;
import java.io.FileNotFoundException;
import java.util.Locale;

/** Отдаёт выбранный файл другому приложению (просмотрщику) без раскрытия file:// пути. */
public class FilesProvider extends ContentProvider {
    public static final String AUTH = "com.yexplorer.app.files";

    public static Uri uriFor(File f) {
        return new Uri.Builder().scheme("content").authority(AUTH).encodedPath(Uri.encode(f.getAbsolutePath(), "/")).build();
    }

    @Override
    public boolean onCreate() { return true; }

    private File resolve(Uri uri) throws FileNotFoundException {
        String p = uri.getPath();
        if (p == null) throw new FileNotFoundException();
        try {
            File f = new File(p).getCanonicalFile();
            String c = f.getPath();
            String prim = Environment.getExternalStorageDirectory().getCanonicalPath();
            boolean allowed = c.startsWith("/storage/") || c.equals(prim) || c.startsWith(prim + "/");
            if (!allowed || !f.isFile()) throw new FileNotFoundException();
            return f;
        } catch (java.io.IOException e) {
            throw new FileNotFoundException();
        }
    }

    @Override
    public Cursor query(Uri uri, String[] projection, String selection, String[] selectionArgs, String sortOrder) {
        try {
            File f = resolve(uri);
            String[] cols = projection != null ? projection : new String[]{OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE};
            MatrixCursor c = new MatrixCursor(cols);
            Object[] row = new Object[cols.length];
            for (int i = 0; i < cols.length; i++) {
                if (OpenableColumns.DISPLAY_NAME.equals(cols[i])) row[i] = f.getName();
                else if (OpenableColumns.SIZE.equals(cols[i])) row[i] = f.length();
            }
            c.addRow(row);
            return c;
        } catch (FileNotFoundException e) {
            return null;
        }
    }

    @Override
    public String getType(Uri uri) {
        String p = uri.getPath();
        if (p != null) {
            int k = p.lastIndexOf('.');
            if (k >= 0) {
                String m = MimeTypeMap.getSingleton().getMimeTypeFromExtension(p.substring(k + 1).toLowerCase(Locale.ROOT));
                if (m != null) return m;
            }
        }
        return "application/octet-stream";
    }

    @Override
    public ParcelFileDescriptor openFile(Uri uri, String mode) throws FileNotFoundException {
        return ParcelFileDescriptor.open(resolve(uri), ParcelFileDescriptor.MODE_READ_ONLY);
    }

    @Override
    public Uri insert(Uri uri, ContentValues values) { return null; }

    @Override
    public int delete(Uri uri, String selection, String[] selectionArgs) { return 0; }

    @Override
    public int update(Uri uri, ContentValues values, String selection, String[] selectionArgs) { return 0; }
}
