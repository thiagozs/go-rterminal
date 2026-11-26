#!/usr/bin/env python3
"""Generates the 16-bit style README images (Sweetie-16 palette, 16 colors).

Usage: python3 docs/images/gen.py   (needs Pillow)
The drawings are made at a tiny logical resolution and upscaled with
nearest-neighbour so every logical pixel becomes a crisp 4x4 block.
"""
import os
import random

from PIL import Image, ImageDraw, ImageFont

SCALE = 4
OUT = os.path.dirname(os.path.abspath(__file__))

# Sweetie-16 palette by Grafxkid: exactly 16 colors.
P = {
    "ink": "#1a1c2c", "plum": "#5d275d", "red": "#b13e53", "orange": "#ef7d57",
    "yellow": "#ffcd75", "lime": "#a7f070", "green": "#38b764", "teal": "#257179",
    "navy": "#29366f", "blue": "#3b5dc9", "sky": "#41a6f6", "ice": "#73eff7",
    "white": "#f4f4f4", "silver": "#94b0c2", "slate": "#566c86", "dusk": "#333c57",
}
PALETTE = list(P.values())
FONT = ImageFont.load_default_imagefont()


def rgb(name):
    c = P.get(name, name)
    return tuple(int(c[i:i + 2], 16) for i in (1, 3, 5))


class Canvas:
    def __init__(self, w, h, bg="ink"):
        self.w, self.h = w, h
        self.img = Image.new("RGB", (w, h), rgb(bg))
        self.d = ImageDraw.Draw(self.img)

    def rect(self, x, y, w, h, fill):
        self.d.rectangle([x, y, x + w - 1, y + h - 1], fill=rgb(fill))

    def frame(self, x, y, w, h, fill, border, shadow="dusk"):
        if shadow:
            self.rect(x + 2, y + 2, w, h, shadow)
        self.rect(x, y, w, h, border)
        self.rect(x + 1, y + 1, w - 2, h - 2, fill)

    def text(self, x, y, s, color="white", scale=1, shadow=None):
        bbox = FONT.getbbox(s)
        tw, th = bbox[2] + 1, bbox[3] + 1
        mask = Image.new("L", (tw, th), 0)
        ImageDraw.Draw(mask).text((0, 0), s, font=FONT, fill=255)
        mask = mask.point(lambda v: 255 if v > 100 else 0)
        if scale > 1:
            mask = mask.resize((tw * scale, th * scale), Image.NEAREST)
        if shadow:
            self.img.paste(Image.new("RGB", mask.size, rgb(shadow)), (x + scale, y + scale), mask)
        self.img.paste(Image.new("RGB", mask.size, rgb(color)), (x, y), mask)
        return tw * scale

    def center(self, cx, y, s, color="white", scale=1, shadow=None):
        w = (FONT.getbbox(s)[2] + 1) * scale
        self.text(cx - w // 2, y, s, color, scale, shadow)

    def arrow(self, x0, y0, x1, y1, color="yellow", both=False):
        c = rgb(color)
        if y0 == y1:  # horizontal, 2px thick
            lo, hi = sorted((x0, x1))
            self.d.rectangle([lo, y0, hi, y0 + 1], fill=c)
            for tip, direction in ((x1, 1 if x1 > x0 else -1), (x0, -1 if x1 > x0 else 1)):
                if tip == x0 and not both:
                    continue
                for i in range(4):
                    px = tip - direction * i
                    self.d.rectangle([px, y0 - (3 - i), px, y0 + 1 + (3 - i)], fill=c)
        else:  # vertical
            lo, hi = sorted((y0, y1))
            self.d.rectangle([x0, lo, x0 + 1, hi], fill=c)
            direction = 1 if y1 > y0 else -1
            for i in range(4):
                py = y1 - direction * i
                self.d.rectangle([x0 - (3 - i), py, x0 + 1 + (3 - i), py], fill=c)

    def badge(self, cx, cy, n, fill="yellow", fg="ink"):
        self.rect(cx - 5, cy - 6, 11, 13, "ink")
        self.rect(cx - 4, cy - 5, 9, 11, fill)
        self.center(cx, cy - 5, str(n), fg)

    def stars(self, seed, count, y_max):
        rnd = random.Random(seed)
        for _ in range(count):
            x, y = rnd.randrange(self.w), rnd.randrange(y_max)
            self.rect(x, y, 1, 1, rnd.choice(["dusk", "slate", "slate", "silver"]))

    def dither(self, y0, y1, c1, c2):
        for y in range(y0, y1):
            for x in range(self.w):
                if (x + y) % 2 == 0:
                    self.d.point((x, y), fill=rgb(c1))
                else:
                    self.d.point((x, y), fill=rgb(c2))

    def swatches(self, x, y, size=7):
        for i, name in enumerate(P):
            self.rect(x + i * size, y, size, size, name)

    def save(self, name):
        out = self.img.resize((self.w * SCALE, self.h * SCALE), Image.NEAREST)
        out.save(os.path.join(OUT, name), optimize=True)


# ---------- sprites ----------
def monitor(c, x, y):
    c.rect(x, y, 26, 18, "silver")
    c.rect(x + 2, y + 2, 22, 14, "ink")
    c.rect(x + 4, y + 4, 8, 2, "lime")
    c.rect(x + 4, y + 8, 14, 2, "lime")
    c.rect(x + 4, y + 12, 3, 2, "yellow")
    c.rect(x + 10, y + 18, 6, 3, "slate")
    c.rect(x + 6, y + 21, 14, 2, "silver")


def rack(c, x, y):
    for i in range(3):
        c.rect(x, y + i * 8, 26, 7, "silver")
        c.rect(x + 1, y + i * 8 + 1, 24, 5, "dusk")
        c.rect(x + 3, y + i * 8 + 3, 2, 2, "lime" if i != 1 else "yellow")
        c.rect(x + 8, y + i * 8 + 3, 12, 1, "slate")


def robot(c, x, y):
    c.rect(x + 12, y, 2, 4, "yellow")
    c.rect(x + 11, y - 1, 4, 2, "red")
    c.rect(x, y + 4, 26, 16, "silver")
    c.rect(x + 2, y + 6, 22, 12, "ink")
    c.rect(x + 5, y + 9, 4, 4, "ice")
    c.rect(x + 17, y + 9, 4, 4, "ice")
    c.rect(x + 9, y + 15, 8, 1, "lime")
    c.rect(x - 2, y + 9, 2, 6, "slate")
    c.rect(x + 26, y + 9, 2, 6, "slate")


def shield(c, x, y):
    rows = [(0, 14), (0, 14), (0, 14), (1, 12), (2, 10), (3, 8), (5, 4)]
    for i, (dx, w) in enumerate(rows):
        c.rect(x + dx, y + i * 2, w, 2, "lime")
        c.rect(x + dx + 1, y + i * 2, max(w - 2, 1), 2, "green")
    c.rect(x + 6, y + 3, 2, 6, "white")


# ---------- images ----------
def sprite(c, x, y, rows, color, px=2):
    for j, row in enumerate(rows):
        for i, ch in enumerate(row):
            if ch == "X":
                c.rect(x + i * px, y + j * px, px, px, color)


HEART = [".XX.XX.", "XXXXXXX", "XXXXXXX", ".XXXXX.", "..XXX..", "...X..."]
HOURGLASS = ["XXXXXXX", ".XXXXX.", "..XXX..", "...X...", "..XXX..", ".XXXXX.", "XXXXXXX"]
STACK = ["XXXXXXX", "XXXXXXX", ".......", "XXXXXXX", "XXXXXXX", "......."]
QUEUE = ["X..X...", "XX.XX..", ".XX.XX.", "..X..X.", ".XX.XX.", "XX.XX..", "X..X..."]


def architecture():
    c = Canvas(400, 225)
    c.stars(7, 90, 225)
    c.rect(0, 0, 400, 24, "dusk")
    c.rect(0, 24, 400, 1, "slate")
    c.text(10, 6, "GO-RTERMINAL", "yellow", 2, "red")
    c.text(222, 8, "REMOTE SHELL OVER WEBSOCKET", "silver")

    # three main boxes
    c.frame(8, 40, 82, 62, "navy", "sky")
    monitor(c, 36, 47)
    c.center(49, 76, "CLIENT", "white")
    c.center(49, 87, "raw tty", "ice")

    c.frame(140, 36, 120, 74, "plum", "orange")
    rack(c, 187, 41)
    c.center(200, 68, "SERVER", "white")
    for i, (label, col) in enumerate((("AUTH", "lime"), ("PAIR", "yellow"), ("RELAY", "ice"))):
        bx = 145 + i * 38
        c.rect(bx, 85, 36, 14, "ink")
        c.center(bx + 18, 87, label, col)

    c.frame(310, 40, 82, 62, "teal", "lime")
    robot(c, 337, 49)
    c.center(351, 76, "AGENT", "white")
    c.center(351, 87, "per host", "lime")

    # arrows: control plane (top, yellow) and data plane (bottom, cyan)
    c.arrow(92, 56, 138, 56, "yellow")
    c.badge(115, 48, 1)
    c.center(115, 60, "HTTP", "silver")
    c.arrow(262, 56, 308, 56, "yellow")
    c.badge(285, 48, 2)
    c.center(285, 60, "CTRL WS", "silver")
    c.arrow(92, 86, 138, 86, "ice", both=True)
    c.badge(115, 78, 3, "ice")
    c.center(115, 90, "WS", "silver")
    c.arrow(262, 86, 308, 86, "ice", both=True)
    c.badge(285, 78, 3, "ice")
    c.center(285, 90, "WS", "silver")

    # agent -> PTY -> shell
    c.arrow(351, 104, 351, 116, "lime")
    c.frame(312, 118, 78, 30, "dusk", "slate")
    c.center(351, 122, "PTY", "yellow")
    c.center(351, 134, "bash / zsh", "silver")
    c.arrow(351, 150, 351, 162, "lime")
    c.frame(312, 164, 78, 30, "ink", "green")
    c.center(351, 168, "$ ls -la_", "lime")
    c.center(351, 180, "SHELL", "silver")

    # quest log
    c.frame(8, 116, 288, 90, "ink", "slate", shadow="dusk")
    c.rect(8, 116, 288, 12, "slate")
    c.text(14, 118, "QUEST LOG: HOW A SESSION IS BORN", "white")
    steps = [
        ("1", "CLIENT asks SERVER for a session"),
        ("2", "SERVER signals AGENT: start-session"),
        ("3", "CLIENT + AGENT both join /session"),
        ("4", "SERVER says paired; AGENT spawns shell"),
        ("5", "bytes + resize flow via the relay"),
    ]
    for i, (n, label) in enumerate(steps):
        y = 134 + i * 14
        c.badge(20, y + 5, n, "yellow" if n in "12" else "ice" if n == "3" else "lime")
        c.text(32, y, label, "white")

    c.text(8, 212, "PALETTE: 16 COLORS", "silver")
    c.swatches(136, 211, 9)
    c.save("architecture.png")


def reliability():
    c = Canvas(400, 225)
    c.stars(11, 90, 225)
    c.rect(0, 0, 400, 24, "dusk")
    c.rect(0, 24, 400, 1, "slate")
    c.text(10, 6, "STATUS SCREEN", "yellow", 2, "red")
    c.text(258, 8, "DEFAULTS FROM THE CODE", "silver")

    # --- left: reconnect backoff chart ---
    c.frame(8, 34, 206, 182, "ink", "slate")
    c.rect(8, 34, 206, 12, "slate")
    c.text(14, 36, "AGENT RECONNECT BACKOFF", "white")
    # upper bound per attempt: min=5s doubling to max=60s (full jitter: delay is 0..bound)
    bounds = [5, 10, 20, 40, 60, 60]
    base_y, top_y, chart_x = 186, 86, 34
    c.text(14, 50, "bound doubles: 5s .. 60s", "silver")
    c.rect(chart_x - 2, top_y - 4, 2, base_y - top_y + 6, "silver")
    c.rect(chart_x - 2, base_y, 170, 2, "silver")
    for sec in (0, 20, 40, 60):
        y = base_y - int(sec / 60 * (base_y - top_y))
        c.text(12, y - 5, f"{sec:>2}", "silver")
        if sec:
            for x in range(chart_x, chart_x + 166, 4):
                c.d.point((x, y), fill=rgb("dusk"))
    colors = ["lime", "lime", "yellow", "orange", "red", "red"]
    for i, (b, col) in enumerate(zip(bounds, colors)):
        h = max(int(b / 60 * (base_y - top_y)), 2)
        x = chart_x + 4 + i * 27
        c.rect(x + 2, base_y - h + 2, 20, h, "dusk")
        c.rect(x, base_y - h, 20, h, col)
        c.rect(x, base_y - h, 20, 2, "white")
        c.center(x + 10, base_y - h - 12, f"{b}s", "white")
        c.center(x + 10, base_y + 5, f"#{i + 1}", "silver")
    c.text(14, 202, "delay = random 0..bound", "ice")

    # --- right: HUD cards ---
    cards = [
        ("shield", "AUTH TOKEN", "REQUIRED", "lime", "no default token"),
        (HEART, "KEEPALIVE", "20s/15s", "ice", "server / agent pings"),
        (HOURGLASS, "PAIRING", "15s", "yellow", "unpaired expires"),
        (STACK, "MAX SHELLS", "8", "orange", "agent busy: close 1013"),
        (QUEUE, "PENDING", "64", "sky", "sessions awaiting peer"),
    ]
    for i, (icon, title, value, col, note) in enumerate(cards):
        y = 34 + i * 37
        c.frame(222, y, 170, 33, "dusk", col)
        if icon == "shield":
            shield(c, 229, y + 9)
        else:
            sprite(c, 229, y + 9, icon, col)
        c.text(250, y + 5, title, "white")
        vw = FONT.getbbox(value)[2] + 1
        c.rect(384 - vw - 3, y + 3, vw + 5, 13, "ink")
        c.text(384 - vw, y + 4, value, col)
        c.text(250, y + 20, note, "silver")
    c.save("reliability.png")


def banner():
    c = Canvas(400, 110)
    # sky gradient with dithering bands
    c.rect(0, 0, 400, 110, "ink")
    c.dither(34, 44, "ink", "dusk")
    c.rect(0, 44, 400, 20, "dusk")
    c.dither(64, 74, "dusk", "navy")
    c.rect(0, 74, 400, 18, "navy")
    c.stars(3, 70, 40)
    # ground
    c.rect(0, 92, 400, 18, "teal")
    for x in range(0, 400, 8):
        c.rect(x, 92, 4, 2, "green")
        c.rect(x + 4, 96, 4, 2, "ink")
    c.dither(98, 110, "teal", "navy")
    # title
    c.center(196, 8, "GO-RTERMINAL", "yellow", 4, "red")
    c.center(200, 58, "REMOTE SHELLS OVER WEBSOCKET", "white", 1)
    c.center(200, 71, "PRESS ENTER TO START", "ice", 1)
    # characters walking the ground
    monitor(c, 30, 62)
    robot(c, 344, 66)
    for x in range(90, 320, 12):  # data packets between client and agent
        c.rect(x, 84, 6, 3, "lime" if (x // 12) % 2 else "yellow")
    c.save("banner.png")


if __name__ == "__main__":
    assert len(PALETTE) == 16
    banner()
    architecture()
    reliability()
    print("ok")
