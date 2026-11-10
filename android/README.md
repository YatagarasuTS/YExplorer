# YExplorer для Android

Быстрый поиск файлов на телефоне: по именам и по содержимому (txt, код, docx/xlsx/pptx/odt и др.), с терпимостью к опечаткам и исправлением раскладки (`ghbdtn` → «привет»). Работает на **Android 8.0+** (API 26). Всё выполняется на устройстве, интернет не нужен.

Не поддерживается на Android: PDF и распознавание текста на скриншотах (OCR). Индекс пересоздаётся при каждом запуске.

## Как получить APK

### Вариант 1. GitHub Actions (без установки чего-либо)
1. Загрузите папку проекта в репозиторий на GitHub.
2. Откройте вкладку **Actions → Build Android APK → Run workflow**.
3. Через 3–5 минут скачайте `YExplorer-apk` из **Artifacts** внизу страницы запуска.
4. Для публикации в Releases создайте тег: `git tag v1.0 && git push --tags` — APK прикрепится к релизу сам.

### Вариант 2. Android Studio
1. **File → Open** и выберите эту папку.
2. Дождитесь синхронизации Gradle (если IDE предложит создать/выбрать Gradle — согласитесь, версия 8.7).
3. **Build → Build Bundle(s) / APK(s) → Build APK(s)**. Файл: `app/build/outputs/apk/debug/`.

Из консоли: `gradle assembleRelease` → `app/build/outputs/apk/release/YExplorer-release.apk`.

## Установка на телефон
Скопируйте APK на телефон, откройте и разрешите установку из неизвестных источников. При первом запуске нажмите «Разрешить доступ» и включите «Доступ ко всем файлам» (Android 11+) или разрешите хранилище (Android 8–10).

## Устройство
- `Engine.java` — индекс и поиск (без зависимостей от Android), `Extract.java` — чтение текста из файлов, `Util.java`, `Json.java`.
- `MainActivity.java` — WebView; запросы `https://yexplorer.app/api/*` обрабатываются в приложении, сеть не используется.
- `FilesProvider.java` — безопасная выдача файла приложению-просмотрщику.
- `app/src/main/assets/index.html` — интерфейс.

Лицензия MIT.
