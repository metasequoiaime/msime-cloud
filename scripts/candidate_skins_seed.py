#!/usr/bin/env python3
"""Render candidate-window skin packages from an msime-skins checkout as idempotent SQL for candidate_skins; never opens a DB connection.

Usage: candidate_skins_seed.py CHECKOUT [--only id,...] [--role ROLE] > candidate-skins.sql

Every package is validated with the rules the client loader applies (crates/client-services/src/skin/catalog.rs), the same port internal/skins/client.go serves them with; testdata/client_dialect.json, a copy of the client's shared case table, pins both. A package the client would reject is refused, never rewritten. Packages whose license.assets says UNVERIFIED are skipped unless named with --only, and then refused. The SQL runs in one transaction as the DML runtime role, inserts only what is missing and fails the whole transaction if an existing package differs from the reviewed files.
"""
import argparse
import hashlib
import math
import os
import re
import subprocess
import sys
import tomllib
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

if __package__:
    from .seed_sql import literal
else:
    from seed_sql import literal

MAX_MANIFEST = 65536
MAX_RESOURCE = 4 << 20
MAX_PACKAGE = 16 << 20
MAX_ENTRIES = 512
RESERVED = {"system", "shuishan", "light", "paper", "night", "ink", "custom"}
# msime-windows 的七个内置外观与它放内置外观设置的 default 目录：外部皮肤不能占用这些 ID。
WINDOWS_LOOKS = {"fluent", "wechat", "graphite", "willow_green", "autumn_osmanthus", "microsoft", "wechat_scroll"}
WINDOWS_DEFAULTS_FOLDER = "default"
CANVAS = "canvas"
BASES = {"system", "shuishan", "light", "paper", "night", "ink"}
# 清单的 base 可以写 msime-windows 内置外观，服务端一律按 system 返回；外观的配色由客户端读清单时补齐。
BASE_ALIASES = {look: "system" for look in WINDOWS_LOOKS}
# 颜色值只放行 CSS 颜色写法用得到的字符，与客户端的 css_color_text 相同。
CSS_COLOR = re.compile(r"[A-Za-z0-9#(),.% /-]*")
CANDIDATE_COLOR_KEYS = ("accent", "selected", "hover", "surface", "border", "text", "number", "translation", "candidate_text", "preedit_text", "preedit_caret", "selected_text", "selected_number", "selected_translation", "selected_bar", "preedit_background", "preedit_divider")
MENU_COLOR_KEYS = ("background", "border", "text", "hover")
MEDIA = {"css": "text/css; charset=utf-8", "png": "image/png", "apng": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "svg": "image/svg+xml", "ico": "image/x-icon", "bmp": "image/bmp", "avif": "image/avif", "woff": "font/woff", "woff2": "font/woff2", "ttf": "font/ttf", "otf": "font/otf"}
BASE_HINT = "base must be system or a built-in theme"
# schema 2 的装饰图待机动效与 [animation] 预设，取值与客户端的 DecorationMotion、SkinAnimationPreset 相同。
DECORATION_MOTIONS = ("none", "float", "sway", "breathe", "bounce")
ANIMATION_PRESETS = ("none", "fade", "fade_scale", "slide_up", "spring", "glow")


class Invalid(Exception):
    """A package the client would not list; the message is the client's reason."""


def media_type(name):
    return MEDIA.get(name.rsplit(".", 1)[-1].lower())


def is_image(name):
    return (media_type(name) or "").startswith("image/")


def safe_id(value):
    # 与 Go 的 SafeID 一致，避免发布在 Windows 上无法按原名安装的目录。
    return value != "nul" and not value.endswith(".") and re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,63}", value) is not None


def safe_resource(name, limit):
    if not name or len(name.encode()) > limit or "\\" in name:
        return False
    return all(part not in ("", ".", "..") and re.fullmatch(r"[A-Za-z0-9._-]+", part) for part in name.split("/"))


def is_int(value):
    return type(value) is int


def within_i64(value):
    if is_int(value):
        return -(1 << 63) <= value < (1 << 63)
    if isinstance(value, dict):
        return all(within_i64(v) for v in value.values())
    if isinstance(value, list):
        return all(within_i64(v) for v in value)
    return True


def required_string(table, key, limit):
    value = table.get(key)
    if not isinstance(value, str):
        raise Invalid(f"{key} must be a string")
    if value == "" or len(value.encode()) > limit:
        raise Invalid(f"{key} has invalid length")
    return value


def optional_string(table, key, limit):
    return required_string(table, key, limit) if key in table else None


def number(table, key):
    if key not in table:
        return 0.0
    value = table[key]
    if is_int(value) or type(value) is float:
        return float(value)
    return math.nan


def bounded(value, limit):
    return math.isfinite(value) and 0 <= value <= limit


def enum_array(table, key, allowed):
    items = table.get(key)
    if not isinstance(items, list) or not items:
        return None
    values = []
    for item in items:
        if not isinstance(item, str) or item not in allowed or item in values:
            return None
        values.append(item)
    return values


def package_files(files):
    """Paths that exist in the package, mapped to whether each is a regular file; directories come from the file paths, as the client sees them on disk."""
    known = {"skin.toml": True}
    for path in files:
        known[path] = True
        while "/" in path:
            path = path.rsplit("/", 1)[0]
            known.setdefault(path, False)
    return known


def validate(folder, manifest, files):
    """校验皮肤包：folder 是 ID，manifest 为原始清单，files 为资源路径和大小；返回清单或按客户端原因抛出 Invalid。"""
    if not safe_id(folder) or folder in RESERVED or folder in WINDOWS_LOOKS or folder == WINDOWS_DEFAULTS_FOLDER or folder == CANVAS:
        raise Invalid("invalid skin id")
    if len(manifest) > MAX_MANIFEST:
        raise Invalid("skin.toml is too large")
    try:
        text = manifest.decode("utf-8")
    except UnicodeDecodeError:
        raise Invalid("skin.toml is not UTF-8") from None
    try:
        table = tomllib.loads(text)
    except tomllib.TOMLDecodeError:
        raise Invalid("invalid TOML") from None
    # The client's TOML parser stores integers as i64 and rejects anything wider as a syntax error.
    if not within_i64(table):
        raise Invalid("invalid TOML")
    schema = table.get("schema_version")
    if not is_int(schema) or schema not in (1, 2):
        raise Invalid("unsupported schema_version")
    if required_string(table, "id", 64) != folder:
        raise Invalid("manifest id does not match folder")
    required_string(table, "name", 80)
    required_string(table, "version", 32)
    base = required_string(table, "base", 32)
    canvas = base == CANVAS
    if canvas and schema != 2:
        raise Invalid("base = canvas requires schema_version 2")
    for key, limit in (("author", 120), ("description", 500)):
        if table.get(key) != "":
            optional_string(table, key, limit)
    base = "system" if canvas else BASE_ALIASES.get(base, base)
    table["base"] = base
    if base not in BASES:
        raise Invalid(BASE_HINT)
    supports = table.get("supports")
    if not isinstance(supports, dict):
        raise Invalid("missing supports")
    if enum_array(supports, "layouts", ("horizontal", "vertical")) is None or enum_array(supports, "themes", ("dark", "light")) is None:
        raise Invalid("invalid supports")
    window = table.get("candidate_window")
    if not isinstance(window, dict):
        raise Invalid("missing candidate_window")
    known = package_files(files)

    def image(name):
        return isinstance(name, str) and safe_resource(name, 256) and is_image(name) and name in known

    if not bounded(number(window, "min_width_dip"), 1000):
        raise Invalid("invalid min_width_dip")
    if "corner_radius_dip" in window and not bounded(number(window, "corner_radius_dip"), 32):
        raise Invalid("invalid corner_radius_dip")
    validate_windows_window_keys(window)
    validate_window_geometry(window, table.get("toolbar"))
    if canvas:
        validate_canvas(window, image)
    decoration = window.get("decoration", {})
    if not isinstance(decoration, dict):
        raise Invalid("invalid decoration")
    top, width = number(decoration, "top_inset_dip"), number(decoration, "width_dip")
    if not bounded(top, 500) or not bounded(width, 1000) or (top == 0) != (width == 0):
        raise Invalid("invalid decoration")
    decoration_image = optional_string(decoration, "image", 256)
    if decoration_image is not None and (top == 0 or not image(decoration_image)):
        raise Invalid("invalid decoration")
    if optional_string(decoration, "align", 16) not in (None, "left", "center", "right"):
        raise Invalid("invalid decoration")
    validate_decoration_motion(decoration, top > 0, schema)
    if "background" in window:
        background = window["background"]
        if not isinstance(background, dict) or not image(background.get("image")):
            raise Invalid("invalid background")
        if "fit" in background and background["fit"] not in ("cover", "contain", "stretch"):
            raise Invalid("invalid background")
        if "opacity" in background and not bounded(number(background, "opacity"), 1):
            raise Invalid("invalid background")
    if "toolbar" in table:
        toolbar = table["toolbar"]
        if not isinstance(toolbar, dict):
            raise Invalid("invalid toolbar")
        if "corner_radius_dip" in toolbar and not bounded(number(toolbar, "corner_radius_dip"), 32):
            raise Invalid("invalid toolbar")
        for mode in ("dark", "light"):
            if mode not in toolbar:
                continue
            colors = toolbar[mode]
            if not isinstance(colors, dict):
                raise Invalid("invalid toolbar")
            for key in ("background", "border", "handle", "divider", "icon", "hover"):
                if key not in colors:
                    continue
                if not isinstance(colors[key], str):
                    raise Invalid("invalid toolbar")
                if len(colors[key].encode()) > 80:
                    raise Invalid("toolbar color exceeds 80 bytes")
                if not CSS_COLOR.fullmatch(colors[key]):
                    raise Invalid("invalid toolbar")
    if "license" in table:
        license_table = table["license"]
        if not isinstance(license_table, dict):
            raise Invalid("invalid license")
        for key, limit in (("code", 120), ("assets", 120), ("source", 500)):
            optional_string(license_table, key, limit)
    stylesheet = optional_string(table, "toolbar_stylesheet", 128)
    if stylesheet is not None and (not safe_resource(stylesheet, 128) or "/" in stylesheet or len(stylesheet) <= 4 or not stylesheet.endswith(".css") or not known.get(stylesheet)):
        raise Invalid("invalid toolbar_stylesheet")
    preview = optional_string(table, "preview", 256)
    if preview is not None and (not safe_resource(preview, 256) or preview not in known):
        raise Invalid("invalid preview")
    validate_colors(table)
    validate_animation(table, schema)
    validate_resources(manifest, files)
    return table


def validate_box(table, key, required=False):
    if key not in table:
        if required:
            raise Invalid("invalid border_image " + key)
        return
    values = table[key]
    if not isinstance(values, list) or len(values) not in (1, 2, 4) or any(
        not (is_int(value) or type(value) is float) or not bounded(value, 500) for value in values
    ):
        raise Invalid("invalid " + key)


def validate_geometry(table):
    validate_box(table, "preedit_margin_dip")
    for key, dip, em in (("padding", 64, 4), ("item_gap", 64, 4), ("item_extra_height", 64, 4),
                         ("number_inset", 64, 4), ("item_padding_right", 64, 4), ("item_min_width", 500, 40),
                         ("number_gap", 64, 4), ("selected_bar_width", 16, 1)):
        if key not in table:
            continue
        value = table[key]
        if isinstance(value, str):
            try:
                n = float(value[:-2]) if value.endswith("em") and re.fullmatch(r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?", value[:-2]) else math.nan
            except ValueError:
                n = math.nan
            valid = bounded(n, em)
        else:
            valid = bounded(number(table, key), dip)
        if not valid:
            raise Invalid("invalid " + key)
    for key in ("number_scale", "selected_bar_height_em"):
        if key in table and not bounded(number(table, key), 2):
            raise Invalid("invalid " + key)
    for key in ("selected_bar_inside", "item_corners_follow_card", "justify_rows", "trim_hidden_preedit_gap"):
        if key in table and type(table[key]) is not bool:
            raise Invalid("invalid " + key)
    if "page_arrow_style" in table and table["page_arrow_style"] not in ("chevron", "triangle"):
        raise Invalid("invalid page_arrow_style")


def validate_window_geometry(window, toolbar):
    if "backdrop" in window and window["backdrop"] not in ("none", "acrylic", "mica"):
        raise Invalid("invalid backdrop")
    for key in ("fit_content", "expand_panel"):
        if key in window and type(window[key]) is not bool:
            raise Invalid("invalid " + key)
    if "expand_rows" in window and (not is_int(window["expand_rows"]) or not 2 <= window["expand_rows"] <= 8):
        raise Invalid("invalid expand_rows")
    validate_geometry(window)
    for key in ("horizontal", "vertical"):
        if key in window:
            if not isinstance(window[key], dict):
                raise Invalid("invalid " + key + " geometry")
            validate_geometry(window[key])
    if toolbar is not None:
        if not isinstance(toolbar, dict):
            raise Invalid("invalid toolbar")
        if "icon_corner_radius_dip" in toolbar and not bounded(number(toolbar, "icon_corner_radius_dip"), 16):
            raise Invalid("invalid icon_corner_radius_dip")


def validate_canvas(window, image):
    def animated(name):
        return image(name) and name.rsplit(".", 1)[-1].lower() in ("png", "apng", "gif", "webp")

    if "border_image" in window:
        border = window["border_image"]
        if not isinstance(border, dict):
            raise Invalid("invalid border_image")
        source = required_string(border, "source", 256)
        if not animated(source):
            raise Invalid("invalid border_image source")
        light = optional_string(border, "light_source", 256)
        if light is not None and not animated(light):
            raise Invalid("invalid border_image light_source")
        for key in ("slice_dip", "stretch_dip", "width_dip"):
            validate_box(border, key, key == "slice_dip")
        if "fill" in border and type(border["fill"]) is not bool:
            raise Invalid("invalid fill")
    for key, vertical, horizontal in (("content_insets_dip", 200, 500), ("outer_insets_dip", 500, 500)):
        if key in window:
            insets = window[key]
            if not isinstance(insets, dict) or any(
                side in insets and not bounded(number(insets, side), limit)
                for side, limit in (("top", vertical), ("right", horizontal), ("bottom", vertical), ("left", horizontal))
            ):
                raise Invalid("invalid " + key)
    if "line_gap_dip" in window and not bounded(number(window, "line_gap_dip"), 100):
        raise Invalid("invalid line_gap_dip")
    if "decorations" not in window:
        return
    values = window["decorations"]
    if not isinstance(values, list):
        raise Invalid("invalid decorations")
    if len(values) > 32:
        raise Invalid("too many decorations")
    for decoration in values:
        if not isinstance(decoration, dict):
            raise Invalid("invalid decoration")
        if not animated(required_string(decoration, "image", 256)):
            raise Invalid("invalid decoration image")
        if decoration.get("anchor", "right") not in ("top-left", "top-right", "bottom-left", "bottom-right", "left", "right", "top", "bottom", "center"):
            raise Invalid("invalid decoration anchor")
        for key in ("offset_x_dip", "offset_y_dip"):
            n = number(decoration, key)
            if not math.isfinite(n) or not -2000 <= n <= 2000:
                raise Invalid("invalid decoration " + key)
        if "z_index" in decoration and (not is_int(decoration["z_index"]) or not -1000 <= decoration["z_index"] <= 1000):
            raise Invalid("invalid decoration z_index")
        for key in ("width_dip", "height_dip"):
            if key not in decoration:
                raise Invalid("missing decoration " + key)
            n = number(decoration, key)
            if not bounded(n, 2000) or n == 0:
                raise Invalid("invalid decoration " + key)


def validate_decoration_motion(decoration, decorated, schema):
    """客户端的 read_decoration_motion：motion 和 motion_period_ms 只在 schema 2 里可写，并且只对有装饰带的包有意义。"""
    if "motion" not in decoration and "motion_period_ms" not in decoration:
        return
    if schema < 2:
        raise Invalid("decoration motion requires schema_version 2")
    if "motion" in decoration and decoration["motion"] not in DECORATION_MOTIONS:
        raise Invalid("invalid decoration motion")
    if not decorated:
        raise Invalid("invalid decoration motion")
    if "motion_period_ms" in decoration:
        period = decoration["motion_period_ms"]
        if not is_int(period) or not 1200 <= period <= 8000:
            raise Invalid("invalid decoration motion_period_ms")


def validate_animation(table, schema):
    """客户端的 read_animation：[animation] 只在 schema 2 里可写，先查时长和错峰，再按 enter、exit、selection 查预设。"""
    if "animation" not in table:
        return
    if schema < 2:
        raise Invalid("animation requires schema_version 2")
    animation = table["animation"]
    if not isinstance(animation, dict):
        raise Invalid("invalid animation")
    for key, low, high in (("duration_ms", 40, 600), ("stagger_ms", 0, 80)):
        if key in animation and (not is_int(animation[key]) or not low <= animation[key] <= high):
            raise Invalid(f"invalid animation {key}")
    for key in ("enter", "exit", "selection"):
        if key in animation and (not isinstance(animation[key], str) or animation[key] not in ANIMATION_PRESETS):
            raise Invalid(f"invalid animation {key}")


def validate_windows_window_keys(window):
    """客户端的 check_windows_window_keys：只有 msime-windows 会画的键也按它的规则校验。"""
    for key, limit in (("border_width_dip", 4), ("item_corner_radius_dip", 16)):
        if key in window and not bounded(number(window, key), limit):
            raise Invalid(f"invalid {key}")
    if "shadow" in window and window["shadow"] not in ("none", "soft", "strong"):
        raise Invalid("invalid shadow")
    if "font_family" in window:
        family = window["font_family"]
        raw = family.encode() if isinstance(family, str) else b""
        if not raw or len(raw) > 64 or any(byte < 0x80 and (byte < 0x20 or byte == 0x7F or byte in b"\"'\\,;{}<>`") for byte in raw):
            raise Invalid("invalid font_family")
    if "page_arrows" in window and not isinstance(window["page_arrows"], bool):
        raise Invalid("invalid page_arrows")


def check_colors(table, keys):
    for key in keys:
        if key not in table:
            continue
        if not isinstance(table[key], str):
            raise Invalid("invalid candidate colors")
        if len(table[key].encode()) > 80:
            raise Invalid("candidate color exceeds 80 bytes")
        if not CSS_COLOR.fullmatch(table[key]):
            raise Invalid("invalid candidate colors")


def validate_colors(table):
    """The client deserializes both palettes before it checks any colour, so a type error anywhere wins over a length or character error; then each mode's colours and menu are checked in the client's order."""
    if "candidate" not in table:
        return
    candidate = table["candidate"]
    if not isinstance(candidate, dict):
        raise Invalid("invalid candidate colors")
    for mode in ("dark", "light"):
        if mode not in candidate:
            continue
        palette = candidate[mode]
        if not isinstance(palette, dict):
            raise Invalid("invalid candidate colors")
        for key in CANDIDATE_COLOR_KEYS[:8]:
            if key in palette and not isinstance(palette[key], str):
                raise Invalid("invalid candidate colors")
        if "show_selected_bar" in palette and not isinstance(palette["show_selected_bar"], bool):
            raise Invalid("invalid candidate colors")
    for mode in ("dark", "light"):
        palette = candidate.get(mode)
        if not isinstance(palette, dict):
            continue
        check_colors(palette, CANDIDATE_COLOR_KEYS)
        if "menu" in palette:
            if not isinstance(palette["menu"], dict):
                raise Invalid("invalid candidate colors")
            check_colors(palette["menu"], MENU_COLOR_KEYS)


def validate_resources(manifest, files):
    """The service's own limits (internal/skins/client.go storedResources), shared with skins_root: 4 MiB a file, 16 MiB and 512 entries a package, skin.toml included."""
    if len(files) + 1 > MAX_ENTRIES:
        raise Invalid("too many resources")
    total = len(manifest)
    for path, size in sorted(files.items()):
        if path == "skin.toml" or media_type(path) is None or not safe_resource(path, 256) or size > MAX_RESOURCE:
            raise Invalid(f"invalid resource: {path}")
        total += size
        if total > MAX_PACKAGE:
            raise Invalid("package exceeds 16 MiB")


def collect(directory):
    """Read a package directory: the manifest bytes, the files the service can store, and the files left out because they are not skin assets."""
    manifest_path = directory / "skin.toml"
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise Invalid("skin.toml is not a regular file")
    if manifest_path.stat().st_size > MAX_MANIFEST:
        raise Invalid("skin.toml is too large")
    files, skipped = {}, []
    for current, dirs, names in os.walk(directory):
        dirs.sort()
        for name in sorted(dirs + names):
            path = Path(current) / name
            rel = path.relative_to(directory).as_posix()
            if path.is_symlink():
                raise Invalid(f"{rel} is a symbolic link")
            if name in dirs or rel == "skin.toml":
                continue
            if not path.is_file():
                raise Invalid(f"{rel} is not a regular file")
            if media_type(rel) is None or not safe_resource(rel, 256):
                skipped.append(rel)
                continue
            if path.stat().st_size > MAX_RESOURCE:
                raise Invalid(f"invalid resource: {rel} exceeds 4 MiB")
            files[rel] = path.read_bytes()
    return manifest_path.read_bytes(), files, skipped


def unverified(table):
    assets = table.get("license", {}).get("assets") if isinstance(table.get("license"), dict) else None
    return isinstance(assets, str) and "UNVERIFIED" in assets.upper()


def sanitize_remote(url):
    """Drop credentials from an https remote so they never land in reviewed SQL."""
    parts = urlsplit(url)
    if parts.scheme and "@" in parts.netloc:
        return urlunsplit(parts._replace(netloc=parts.netloc.rsplit("@", 1)[1]))
    return url


def provenance(checkout):
    def git(*args):
        return subprocess.run(["git", "-C", str(checkout), *args], capture_output=True, text=True, timeout=30, check=True).stdout.strip()

    try:
        commit = git("rev-parse", "HEAD")
    except (OSError, subprocess.SubprocessError):
        return ["-- Source: not a git checkout"]
    try:
        remote = sanitize_remote(git("remote", "get-url", "origin"))
    except (OSError, subprocess.SubprocessError):
        remote = "(no origin remote)"
    lines = [f"-- Source: {remote} at commit {commit}"]
    if git("status", "--porcelain", "--untracked-files=all"):
        lines.append("-- WARNING: the checkout has uncommitted changes; this SQL reflects the files on disk, not the commit above.")
    return lines


def sha256(raw):
    return hashlib.sha256(raw).hexdigest()


def render(packages, role, source_lines):
    """packages: sorted (id, manifest bytes, {path: bytes}) tuples that already passed validate."""
    out = ["-- Generated by scripts/candidate_skins_seed.py; review before running with psql -X -v ON_ERROR_STOP=1.", *source_lines,
           "BEGIN;", f"SET LOCAL ROLE {role};", "SET LOCAL lock_timeout = '5s';", "SET LOCAL statement_timeout = '60s';",
           "SELECT pg_advisory_xact_lock(hashtext('candidate_skins_seed'));"]
    for sid, manifest, files in packages:
        skin = literal(sid)
        total = len(manifest) + sum(len(raw) for raw in files.values())
        out.append(f"-- Package {sid}: skin.toml sha256 {sha256(manifest)}, {len(files)} other files, {total} bytes")
        out.append(f"INSERT INTO candidate_skins(id,manifest) VALUES({skin},decode('{manifest.hex()}','hex')) ON CONFLICT(id) DO NOTHING;")
        for path, raw in sorted(files.items()):
            out.append(f"-- {path}: {len(raw)} bytes, sha256 {sha256(raw)}")
            out.append(f"INSERT INTO candidate_skin_resources(skin_id,path,bytes) VALUES({skin},{literal(path)},decode('{raw.hex()}','hex')) ON CONFLICT(skin_id,path) DO NOTHING;")
        checks = [f"NOT EXISTS (SELECT 1 FROM candidate_skins WHERE id={skin} AND manifest_sha256='{sha256(manifest)}')",
                  f"(SELECT count(*) FROM candidate_skin_resources WHERE skin_id={skin}) <> {len(files)}"]
        if files:
            wanted = ",".join(f"({literal(path)},'{sha256(raw)}')" for path, raw in sorted(files.items()))
            checks.append(f"EXISTS (SELECT 1 FROM (VALUES {wanted}) AS want(path,sha256) WHERE NOT EXISTS (SELECT 1 FROM candidate_skin_resources r WHERE r.skin_id={skin} AND r.path=want.path AND r.sha256=want.sha256))")
        out.append(f"DO $$ BEGIN IF {' OR '.join(checks)} THEN RAISE EXCEPTION 'Candidate skin % differs from the reviewed package; publish it under a new id instead of overwriting', {skin}; END IF; END $$;")
    out += ["SELECT id, manifest_sha256, published FROM candidate_skins ORDER BY id;", "COMMIT;"]
    return "\n".join(out) + "\n"


def build(checkout, only=None):
    """Validate the checkout's packages. Returns (packages, notes, errors): errors non-empty means nothing may be rendered."""
    checkout = Path(checkout)
    if not checkout.is_dir():
        return [], [], [f"{checkout} is not a directory"]
    found = sorted(p.name for p in checkout.iterdir() if p.is_dir() and not p.name.startswith(".") and (p / "skin.toml").exists())
    notes, errors, packages = [], [], []
    wanted = found if only is None else only
    for sid in sorted(set(wanted) - set(found)):
        errors.append(f"{sid}: no package directory with a skin.toml")
    for sid in [s for s in found if s in wanted]:
        try:
            manifest, files, skipped = collect(checkout / sid)
            table = validate(sid, manifest, {path: len(raw) for path, raw in files.items()})
        except Invalid as error:
            reason = str(error)
            if reason == BASE_HINT:
                reason += " (use system, shuishan, light, paper, night or ink, or an msime-windows look: fluent, wechat, graphite, willow_green, autumn_osmanthus or microsoft)"
            errors.append(f"{sid}: {reason}")
            continue
        if unverified(table):
            if only is None:
                notes.append(f"{sid}: skipped, license.assets is unverified")
            else:
                errors.append(f"{sid}: license.assets is unverified; only packages with verified asset rights may be published")
            continue
        notes += [f"{sid}: left out {path} (not a skin asset)" for path in skipped]
        packages.append((sid, manifest, files))
    if not packages and not errors:
        errors.append("no packages to publish")
    return packages, notes, errors


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("checkout", help="path to an msime-skins checkout")
    parser.add_argument("--only", help="comma-separated package ids to publish")
    parser.add_argument("--role", default="msime_backend", help="DML runtime role the SQL switches to (default msime_backend)")
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[a-z_][a-z0-9_]{0,62}", args.role):
        parser.error("--role must be a plain lowercase PostgreSQL role name")
    only = None if args.only is None else [s.strip() for s in args.only.split(",") if s.strip()]
    packages, notes, errors = build(args.checkout, only)
    for line in notes:
        print(f"candidate_skins_seed: {line}", file=sys.stderr)
    if errors:
        for line in errors:
            print(f"candidate_skins_seed: refused {line}", file=sys.stderr)
        return 1
    sys.stdout.write(render(packages, args.role, provenance(Path(args.checkout))))
    return 0


if __name__ == "__main__":
    sys.exit(main())
