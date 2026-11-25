#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="proxy2proxy"
BUNDLE_ID="local.di35e1.proxy2proxy"
TARGET_DIR="$HOME/Applications/proxy2proxy"
OUT_APP="${TARGET_DIR}/${APP_NAME}.app"
BIN_PATH="${OUT_APP}/Contents/MacOS/${APP_NAME}"
TMP_BIN="${SCRIPT_DIR}/proxy2proxy_arm64"

export MACOSX_DEPLOYMENT_TARGET="12.0"
export CGO_CFLAGS="-mmacosx-version-min=12.0"
export CGO_LDFLAGS="-mmacosx-version-min=12.0"

cd "$SCRIPT_DIR"

echo "1/3. Останавливаем запущенный процесс и очищаем старую сборку..."
pkill -f "$BIN_PATH" 2>/dev/null || true
rm -f "${TMP_BIN}"
rm -rf "${OUT_APP}"

echo "2/3. Сборка под arm64 (Apple Silicon)..."
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -o "${TMP_BIN}" .

echo "3/3. Упаковка в ${OUT_APP} и копирование ресурсов..."
mkdir -p "${OUT_APP}/Contents/MacOS" "${OUT_APP}/Contents/Resources"
mv "${TMP_BIN}" "${BIN_PATH}"
chmod +x "${BIN_PATH}"

cat > "${OUT_APP}/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleDevelopmentRegion</key>
    <string>en</string>
    <key>CFBundleExecutable</key>
    <string>${APP_NAME}</string>
    <key>CFBundleIdentifier</key>
    <string>${BUNDLE_ID}</string>
    <key>CFBundleInfoDictionaryVersion</key>
    <string>6.0</string>
    <key>CFBundleName</key>
    <string>${APP_NAME}</string>
    <key>CFBundleDisplayName</key>
    <string>${APP_NAME}</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>CFBundleVersion</key>
    <string>1</string>
    <key>LSMinimumSystemVersion</key>
    <string>12.0</string>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
EOF

if [ -f "${SCRIPT_DIR}/AppIcon.icns" ]; then
    cp "${SCRIPT_DIR}/AppIcon.icns" "${OUT_APP}/Contents/Resources/AppIcon.icns"
    /usr/libexec/PlistBuddy -c "Add :CFBundleIconFile string AppIcon" "${OUT_APP}/Contents/Info.plist"
    echo "Иконка подключена: ${OUT_APP}/Contents/Resources/AppIcon.icns"
else
    echo "Внимание: ${SCRIPT_DIR}/AppIcon.icns не найден, сборка без кастомной иконки."
fi

if [ -f "${SCRIPT_DIR}/config.json" ]; then
    if [ ! -f "${TARGET_DIR}/config.json" ]; then
        cp "${SCRIPT_DIR}/config.json" "${TARGET_DIR}/config.json"
        echo "Скопирован ${TARGET_DIR}/config.json"
    elif cmp -s "${SCRIPT_DIR}/config.json" "${TARGET_DIR}/config.json"; then
        echo "Конфиг уже актуален: ${TARGET_DIR}/config.json"
    elif [ -t 0 ]; then
        printf 'Перезаписать %s конфигом из репозитория? [y/N] ' "${TARGET_DIR}/config.json"
        read -r answer || answer=""
        case "$answer" in
            y|Y|yes|YES|Yes)
                cp "${SCRIPT_DIR}/config.json" "${TARGET_DIR}/config.json"
                echo "Скопирован ${TARGET_DIR}/config.json"
                ;;
            *)
                echo "Оставлен существующий ${TARGET_DIR}/config.json"
                ;;
        esac
    else
        echo "Оставлен существующий ${TARGET_DIR}/config.json (нет интерактивного ввода)"
    fi
elif [ -f "${SCRIPT_DIR}/config.example.json" ] && [ ! -f "${TARGET_DIR}/config.json" ]; then
    cp "${SCRIPT_DIR}/config.example.json" "${TARGET_DIR}/config.json"
    echo "Скопирован шаблон в ${TARGET_DIR}/config.json"
fi

if [ -f "${TARGET_DIR}/config.json" ]; then
    chmod 600 "${TARGET_DIR}/config.json"
fi

codesign --force --deep --sign - "${OUT_APP}"
touch "${OUT_APP}"

echo "Готово: ${OUT_APP}"
lipo -info "${BIN_PATH}"