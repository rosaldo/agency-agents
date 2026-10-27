"""scene.json schema and the rules that keep motion-studio output from looking generated.

validate(scene, base_dir) returns a list of {"rule", "where", "message"}
errors; an empty list means the scene may be rendered. references/scene-schema.md
documents every field; keep the two in step.
"""
import math
import re
from fractions import Fraction
from pathlib import Path

from motion_math import beat_frame

# Aspect-ratio presets. Every quality fixes the SHORT side; the long side
# follows the ratio. All 21 sizes are exact, even and distinct.
PRESETS = {"1:1": (1, 1), "9:16": (9, 16), "16:9": (16, 9), "4:5": (4, 5),
           "5:4": (5, 4), "2:3": (2, 3), "3:2": (3, 2)}
QUALITIES = ("low", "medium", "high")
SHORT_SIDE = {"low": 720, "medium": 1080, "high": 1440}
DEFAULT_QUALITY = "medium"

SIZE_LIMITS = (160, 4096)       # per side, pixels
FPS_LIMITS = (12, 60)
BPM_LIMITS = (40, 240)
DURATION_LIMITS = (1, 300)      # seconds
STAGGER_FRAMES = (2, 4)
MAX_FONT_SIZES = 2

# Text size tokens, as a fraction of the frame's short side (never pixels).
TEXT_SIZES = {"xs": 0.028, "s": 0.036, "m": 0.048, "l": 0.072, "xl": 0.11}
ALIGNS = ("start", "center", "end")

# component -> (required fields, text fields, default size per text role)
COMPONENTS = {
    "title":      (("text",), ("text", "subtitle"), {"text": "xl", "subtitle": "m"}),
    "text":       (("text",), ("text",), {"text": "m"}),
    "counter":    (("from", "to"), ("prefix", "suffix"), {"value": "xl"}),
    "list":       (("items",), ("items",), {"items": "m"}),
    "asset-card": (("asset",), ("caption",), {"caption": "s"}),
    "bar-chart":  (("bars",), (), {"labels": "s"}),
    "shape":      ((), (), {}),
    "draw":       (("stroke",), (), {}),
    "morph":      (("from", "to"), (), {}),
    "icon":       (("icon",), (), {}),
    "ring":       (("value",), (), {}),
    "line-chart": (("points",), (), {}),
    "burst":      ((), (), {}),
}

# Graphics drawn in code (lib/shapes.js and lib/components.js mirror these).
# A graphic's `size` is its height in the column layout, as a fraction of the
# short side; it is not a font size.
GRAPHIC_SIZES = {"xs": 0.1, "s": 0.16, "m": 0.24, "l": 0.34, "xl": 0.46}
SHAPES = ("circle", "blob", "rect", "triangle", "hexagon", "star", "heart")
ICONS = ("check", "clock", "chart", "cart", "star", "heart", "arrow", "user", "bolt", "gift", "bell", "calendar")
STROKES = ("underline", "arrow", "connector", "outline", "circle", "wave")
REVEALS = ("wipe", "iris", "slide")
SIDES = ("left", "right", "top", "bottom")
MAX_PALETTE = 6

# Safe area: margins as fractions of the frame (top, right, bottom, left) that
# text must stay inside. A house layout default, not any platform's spec; set
# "safe_area" in scene.json from the target platform's current guide.
# Tall frames keep more room top and bottom, where vertical feeds draw their UI.
SAFE_AREA = {"tall": {"top": 0.08, "right": 0.06, "bottom": 0.12, "left": 0.06},
             "other": {"top": 0.05, "right": 0.05, "bottom": 0.05, "left": 0.05}}
TALL_RATIO = 1.5      # height / width at or above this is "tall" (9:16, 2:3)
SAFE_AREA_MAX = 0.4   # no single margin may eat more than 40% of the frame

HEX = re.compile(r"^#[0-9a-fA-F]{6}$")
EPS = 1e-6


def resolve_size(scene):
    """(width, height) from a preset + quality or from explicit width/height."""
    if "preset" in scene:
        w, h = PRESETS[scene["preset"]]
        short = SHORT_SIDE[scene.get("quality", DEFAULT_QUALITY)]
        scale = Fraction(short, min(w, h))
        return int(w * scale), int(h * scale)
    return scene["width"], scene["height"]


def safe_margins(scene):
    """Margins text must keep: the format default, overridden side by side by scene["safe_area"]."""
    w, h = resolve_size(scene)
    margins = dict(SAFE_AREA["tall" if h / w >= TALL_RATIO else "other"])
    margins.update(scene.get("safe_area") or {})
    return margins


def fmt(x):
    return f"{x:g}"


class Errors(list):
    def add(self, rule, where, message):
        self.append({"rule": rule, "where": where, "message": message})


def _number(v):
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def _in(v, limits):
    return _number(v) and limits[0] <= v <= limits[1]


def _check_format(scene, errors):
    if "preset" in scene:
        if scene["preset"] not in PRESETS:
            errors.add("format", "preset", f"unknown preset {scene['preset']!r}; use one of {', '.join(PRESETS)}")
        if scene.get("quality", DEFAULT_QUALITY) not in QUALITIES:
            errors.add("format", "quality", f"unknown quality {scene['quality']!r}; use one of {', '.join(QUALITIES)}")
        return
    w, h = scene.get("width"), scene.get("height")
    if not (isinstance(w, int) and isinstance(h, int)):
        errors.add("format", "width/height", "give a preset or integer width and height")
        return
    if not (_in(w, SIZE_LIMITS) and _in(h, SIZE_LIMITS)):
        errors.add("size-limits", "width/height", f"{w}x{h}: each side must be {SIZE_LIMITS[0]}-{SIZE_LIMITS[1]} px")
    if w % 2 or h % 2:
        errors.add("even-size", "width/height", f"{w}x{h}: yuv420p needs even width and height")


def _on_beat(t, bpm):
    beats = t * bpm / 60
    return abs(beats - round(beats)) < EPS, round(beats)


def _check_timing(scene, errors):
    fps, bpm, duration = scene.get("fps", 30), scene.get("bpm"), scene.get("duration")
    ok = True
    if not (isinstance(fps, int) and _in(fps, FPS_LIMITS)):
        errors.add("fps", "fps", f"fps must be an integer {FPS_LIMITS[0]}-{FPS_LIMITS[1]}")
        ok = False
    if not _in(bpm, BPM_LIMITS):
        errors.add("bpm", "bpm", f"bpm must be {BPM_LIMITS[0]}-{BPM_LIMITS[1]}")
        ok = False
    if not _in(duration, DURATION_LIMITS):
        errors.add("duration", "duration", f"duration must be {DURATION_LIMITS[0]}-{DURATION_LIMITS[1]} s")
        ok = False
    if ok and not _on_beat(duration, bpm)[0]:
        beat = 60 / bpm
        lower = math.floor(duration / beat) * beat
        errors.add("beat-grid", "duration",
                   f"{fmt(duration)} s is not a whole number of beats at {fmt(bpm)} BPM; "
                   f"use {fmt(round(lower, 6))} or {fmt(round(lower + beat, 6))} s")
    return ok


def _text_values(el, field):
    v = el.get(field)
    if isinstance(v, list):
        return [x for x in v if isinstance(x, str)]
    return [v] if isinstance(v, str) else []


def _check_element(el, where, base_dir, scene_gradient, sizes, errors):
    name = el.get("component")
    if name not in COMPONENTS:
        errors.add("component", where, f"unknown component {name!r}; use one of {', '.join(COMPONENTS)}")
        return
    required, text_fields, default_sizes = COMPONENTS[name]
    for field in required:
        if field not in el:
            errors.add("component", where, f"{name} needs {field!r}")
    size = el.get("size")
    if size is not None and size not in TEXT_SIZES:
        errors.add("font-sizes", where, f"unknown size {size!r}; use one of {', '.join(TEXT_SIZES)}")
    for role, default in default_sizes.items():
        if role in text_fields and role not in el:
            continue  # an optional text that is absent sets no size
        # `size` overrides the main role; secondary roles keep their default
        main = role == next(iter(default_sizes))
        sizes.add(size if (main and size in TEXT_SIZES) else default)
    align = el.get("align", "start")
    if align not in ALIGNS:
        errors.add("component", where, f"align must be one of {', '.join(ALIGNS)}")
    texts = [t for f in text_fields for t in _text_values(el, f)]
    caps = el.get("caps") is True or any(any(c.isalpha() for c in t) and t == t.upper() for t in texts)
    if caps and align == "center" and scene_gradient:
        errors.add("caps-center-gradient", where, "all-caps, centered text on a gradient reads as generated; drop one of the three")
    asset = el.get("asset")
    if el.get("hero") and name != "asset-card":
        errors.add("hero-asset", where, "the hero must be an asset-card showing a real file from assets/")
    if asset is not None:
        path = Path(str(asset))
        real = (path.parts[:1] == ("assets",) and ".." not in path.parts and (base_dir / path).is_file())
        if not real:
            errors.add("hero-asset" if el.get("hero") else "asset", where, f"{asset!r} is not an existing file under assets/")
    box = el.get("box")
    if box is not None:
        valid = (isinstance(box, list) and len(box) == 4 and all(_in(v, (0, 1)) for v in box)
                 and box[0] + box[2] <= 1 + EPS and box[1] + box[3] <= 1 + EPS and box[2] > 0 and box[3] > 0)
        if not valid:
            errors.add("relative-box", where, "box is [x, y, w, h] as fractions 0-1 of the frame, inside the frame")
    for message in _graphic_errors(name, el):
        errors.add("component", where, message)
    spring = el.get("spring")
    if spring is not None and not (isinstance(spring, dict) and _in(spring.get("zeta"), (0.2, 2))
                                   and _in(spring.get("omega"), (4, 40))):
        errors.add("spring", where, "spring needs zeta 0.2-2 and omega 4-40 rad/s")


def _int_in(v, lo, hi):
    return isinstance(v, int) and not isinstance(v, bool) and lo <= v <= hi


def _hex(v):
    return isinstance(v, str) and bool(HEX.match(v))


# field -> (test, what it must be); only checked when the field is present
_GRAPHIC_FIELDS = {
    "color": (_hex, "a #rrggbb colour"),
    "reveal": (lambda v: v in REVEALS, f"one of {', '.join(REVEALS)}"),
}
_GRAPHIC_BY_COMPONENT = {
    "shape": {"shape": (lambda v: v in SHAPES, f"one of {', '.join(SHAPES)}"),
              "from": (lambda v: v in SIDES, f"one of {', '.join(SIDES)}"),
              "spin": (lambda v: _in(v, (-720, 720)), "degrees -720 to 720"),
              "opacity": (lambda v: _in(v, (0.05, 1)), "0.05-1")},
    "draw": {"stroke": (lambda v: v in STROKES, f"one of {', '.join(STROKES)}")},
    "morph": {"from": (lambda v: v in SHAPES, f"one of {', '.join(SHAPES)}"),
              "to": (lambda v: v in SHAPES, f"one of {', '.join(SHAPES)}"),
              "after": (lambda v: _int_in(v, 0, 8), "beats 0-8 after the entrance")},
    "icon": {"icon": (lambda v: v in ICONS, f"one of {', '.join(ICONS)}"),
             "mode": (lambda v: v in ("draw", "pop"), "draw or pop"),
             "disc": (lambda v: isinstance(v, bool), "true or false")},
    "ring": {"value": (lambda v: _in(v, (0, 100)), "a number 0-100")},
    "line-chart": {"points": (lambda v: isinstance(v, list) and 2 <= len(v) <= 12 and all(_number(x) for x in v),
                              "2-12 numbers")},
    "burst": {"count": (lambda v: _int_in(v, 8, 80), "an integer 8-80"),
              "seed": (lambda v: _int_in(v, 0, 2**31), "an integer >= 0")},
}


def _graphic_errors(name, el):
    fields = {**_GRAPHIC_FIELDS, **_GRAPHIC_BY_COMPONENT.get(name, {})}
    return [f"{name}.{field} must be {what}" for field, (ok, what) in fields.items()
            if field in el and not ok(el[field])]


def _check_background(bg, where, errors):
    if bg is None:
        return False
    if isinstance(bg, dict):
        if "shapes" in bg and not _int_in(bg["shapes"], 0, 6):
            errors.add("background", where, "background.shapes is 0-6 soft parallax shapes")
        if "grain" in bg and not isinstance(bg["grain"], bool):
            errors.add("background", where, "background.grain is true or false")
    if isinstance(bg, dict) and HEX.match(str(bg.get("color", ""))):
        return False
    g = bg.get("gradient") if isinstance(bg, dict) else None
    if isinstance(g, list) and len(g) >= 2 and all(HEX.match(str(c)) for c in g):
        return True
    errors.add("background", where, 'background is {"color": "#rrggbb"} or {"gradient": ["#rrggbb", ...]}')
    return False


def _check_scenes(scene, base_dir, timing_ok, errors):
    scenes = scene.get("scenes")
    if not isinstance(scenes, list) or not scenes:
        errors.add("timeline", "scenes", "scenes must be a non-empty list")
        return
    fps, bpm, duration = scene.get("fps", 30), scene.get("bpm"), scene.get("duration")
    top_gradient = _check_background(scene.get("background"), "background", errors)
    previous_end = 0
    for i, sc in enumerate(scenes):
        where = f"scenes[{i}]"
        start, end = sc.get("start"), sc.get("end")
        if not (_number(start) and _number(end) and end > start):
            errors.add("timeline", where, "start and end are seconds with end > start")
            continue
        if abs(start - previous_end) > EPS:
            errors.add("timeline", where, f"starts at {fmt(start)} s but the previous scene ends at {fmt(previous_end)} s")
        previous_end = end
        gradient = _check_background(sc["background"], where, errors) if "background" in sc else top_gradient
        stagger = sc.get("stagger", 3)
        if not (isinstance(stagger, int) and STAGGER_FRAMES[0] <= stagger <= STAGGER_FRAMES[1]):
            errors.add("stagger", where, f"stagger is {STAGGER_FRAMES[0]}-{STAGGER_FRAMES[1]} frames between entrances")
        if timing_ok:
            for t, label in ((start, "start"), (end, "end")):
                on, n = _on_beat(t, bpm)
                if not on:
                    errors.add("cut-on-beat", f"{where}.{label}",
                               f"{fmt(t)} s is between beats; nearest beat is {fmt(round(n * 60 / bpm, 6))} s")
        elements = sc.get("elements") or []
        sizes = set()
        for j, el in enumerate(elements):
            _check_element(el if isinstance(el, dict) else {}, f"{where}.elements[{j}]", base_dir, gradient, sizes, errors)
        if len(sizes) > MAX_FONT_SIZES:
            errors.add("font-sizes", where, f"{len(sizes)} font sizes ({', '.join(sorted(sizes))}); keep at most {MAX_FONT_SIZES}")
        hold = sc.get("hold")
        if hold is not None and timing_ok:
            beats = round((end - start) * bpm / 60)
            if not (isinstance(hold, int) and not isinstance(hold, bool) and 1 <= hold <= beats):
                errors.add("hold", where, f"hold is a whole number of beats, 1-{beats}, at the end of the scene")
        enter_beat = sc.get("enter_beat", 0)
        if timing_ok and isinstance(enter_beat, int) and enter_beat >= 0 and elements and isinstance(stagger, int):
            last = beat_frame(round(start * bpm / 60) + enter_beat, bpm, fps) + (len(elements) - 1) * stagger
            if last >= round(end * fps):
                errors.add("enter-timing", where, "the last element enters after the scene ends; lower enter_beat or split the scene")
        elif not (isinstance(enter_beat, int) and enter_beat >= 0):
            errors.add("enter-timing", where, "enter_beat is a whole beat count >= 0 from the scene start")
    if timing_ok and abs(previous_end - duration) > EPS:
        errors.add("timeline", "scenes", f"scenes end at {fmt(previous_end)} s but duration is {fmt(duration)} s")


def _check_safe_area(scene, errors):
    area = scene.get("safe_area")
    if area is None:
        return
    sides = set(SAFE_AREA["other"])
    if not (isinstance(area, dict) and set(area) <= sides and all(_in(v, (0, SAFE_AREA_MAX)) for v in area.values())):
        errors.add("safe-area", "safe_area",
                   f"safe_area takes {', '.join(sorted(sides))} as frame fractions 0-{SAFE_AREA_MAX}")


def validate(scene, base_dir):
    errors = Errors()
    if not isinstance(scene, dict):
        errors.add("format", "", "scene.json must hold a JSON object")
        return errors
    _check_format(scene, errors)
    timing_ok = _check_timing(scene, errors)
    _check_scenes(scene, Path(base_dir), timing_ok, errors)
    _check_safe_area(scene, errors)
    palette = scene.get("palette")
    if palette is not None and not (isinstance(palette, list) and 1 <= len(palette) <= MAX_PALETTE
                                    and all(_hex(c) for c in palette)):
        errors.add("palette", "palette", f"palette is 1-{MAX_PALETTE} #rrggbb colours for the graphics")
    return errors
