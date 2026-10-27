"""Review layer (b) arithmetic: the contact-sheet grid and the re-render pass limit."""
import math

SHEET_FPS = 4       # cells per second of video
CELL_WIDTH = 270    # pixels, before any shrinking
MAX_SIDE = 1280     # 12 s of 9:16 keeps 128 px cells: layout, graphics and l/xl text still read, at 39% of the 2048 pixels
MAX_PASSES = 2      # re-render passes after the first review


def even(x):
    return max(2, int(x) // 2 * 2)


def grid(cells, width, height):
    """(cols, rows, (cell_w, cell_h)) for a near-square sheet no larger than MAX_SIDE."""
    cw, ch = CELL_WIDTH, even(CELL_WIDTH * height / width)
    cols = max(1, math.ceil(math.sqrt(cells * ch / cw)))
    rows = math.ceil(cells / cols)
    shrink = min(1, MAX_SIDE / (cols * cw), MAX_SIDE / (rows * ch))
    if shrink < 1:
        cw = even(cw * shrink)
        ch = even(cw * height / width)
    return cols, rows, (cw, ch)


def next_pass(done):
    """Number of the next re-render pass; refuses once MAX_PASSES have run."""
    if done >= MAX_PASSES:
        raise ValueError(f"{MAX_PASSES} re-render passes already ran; stop and explain the low scenes")
    return done + 1
