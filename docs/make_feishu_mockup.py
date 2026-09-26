#!/usr/bin/env python3
"""Render 6 frames of the feishu-bridge-go UX redesign as a single PNG.

Mental model (corrected, per user review):
    Main chat = session management console (command-only).
    Topic     = claude session (1:1).

    All claude session management (new/cd/model/stop/status/resume) happens
    in the main chat via commands. Topics only accept normal messages that
    get sent to claude. Card buttons (⏹/📊/❓) inside a topic are the same
    commands re-exposed as shortcuts.

Windows 10 without SegoeUIEmoji.ttf renders unicode emoji as tofu, so this
mockup avoids emoji entirely.

Run:
    python make_feishu_mockup.py
"""
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

OUT = Path(__file__).with_name("feishu-redesign-mockup.png")

BG_SIDEBAR = (26, 26, 26)
BG_CHAT = (247, 247, 248)
CARD_BG = (255, 255, 255)
CARD_BORDER = (225, 225, 230)
CARD_TITLE = (20, 20, 20)
CARD_TEXT = (60, 60, 60)
CARD_DIM = (140, 140, 140)
CODE_BG = (238, 238, 240)
CODE_TEXT = (30, 30, 30)
BUTTON_BG = (245, 245, 247)
BUTTON_BORDER = (215, 215, 220)
BUTTON_TEXT = (40, 40, 40)
USER_BUBBLE = (225, 245, 255)
USER_TEXT = (20, 20, 20)
ACCENT = (44, 116, 255)
GREEN = (76, 175, 80)
YELLOW = (230, 160, 20)
RED = (200, 60, 60)
PURPLE = (124, 77, 255)

FRAME_W = 920
FRAME_H = 340
SIDEBAR_W = 100

FONT_SIZE_TITLE = 15
FONT_SIZE_TEXT = 13
FONT_SIZE_SMALL = 11
FONT_SIZE_BTN = 12

FONT_PATH = "C:/Windows/Fonts/msyh.ttc"
FONT_PATH_BOLD = "C:/Windows/Fonts/msyhbd.ttc"
FONT_MONO = "C:/Windows/Fonts/consola.ttf"
FONT_PATH_FALLBACK = "C:/Windows/Fonts/arial.ttf"


def load_font(size, bold=False, mono=False):
    if mono:
        try:
            return ImageFont.truetype(FONT_MONO, size)
        except Exception:
            pass
    p = FONT_PATH_BOLD if bold else FONT_PATH
    try:
        return ImageFont.truetype(p, size, index=1 if bold else 0)
    except Exception:
        try:
            return ImageFont.truetype(FONT_PATH_FALLBACK, size)
        except Exception:
            return ImageFont.load_default()


F_TITLE = load_font(FONT_SIZE_TITLE)
F_TITLE_B = load_font(FONT_SIZE_TITLE, bold=True)
F_TEXT = load_font(FONT_SIZE_TEXT)
F_TEXT_B = load_font(FONT_SIZE_TEXT, bold=True)
F_SMALL = load_font(FONT_SIZE_SMALL)
F_BTN = load_font(FONT_SIZE_BTN)
F_MONO = load_font(FONT_SIZE_TEXT, mono=True)


def draw_badge(draw, x, y, size, color, shape="square"):
    if shape == "square":
        draw.rounded_rectangle([x, y, x + size, y + size], radius=2, fill=color)
    elif shape == "circle":
        draw.ellipse([x, y, x + size, y + size], fill=color)
    elif shape == "bar":
        draw.rectangle([x, y + size * 0.25, x + size, y + size * 0.38], fill=color)
        draw.rectangle([x, y + size * 0.6, x + size, y + size * 0.72], fill=color)


def draw_bot_avatar(draw, x, y, size=20):
    draw.rounded_rectangle([x, y, x + size, y + size], radius=5, fill=ACCENT)
    cx, cy = x + size // 2, y + size // 2
    draw.rectangle([cx - 4, cy - 2, cx - 2, cy], fill=(255, 255, 255))
    draw.rectangle([cx + 2, cy - 2, cx + 4, cy], fill=(255, 255, 255))
    draw.rectangle([cx - 3, cy + 3, cx + 3, cy + 4], fill=(255, 255, 255))


def card_height(body_lines, buttons):
    top = 12
    title_h = 22
    body_h = sum(20 for _ in body_lines)
    btn_h = 40 if buttons else 0
    return top * 2 + title_h + body_h + btn_h + 12


def draw_bot_card(draw, x, y, w, title, body_lines, buttons=None,
                   badge_shape="square", badge_color=ACCENT):
    h = card_height(body_lines, buttons or ())
    draw.rounded_rectangle([x, y, x + w, y + h], radius=10, fill=CARD_BG, outline=CARD_BORDER, width=1)

    draw_bot_avatar(draw, x + 12, y + 12, size=20)

    label_y = y + 15
    draw.text((x + 40, label_y), "claude-pc", font=F_TITLE, fill=CARD_TITLE)
    tw = draw.textlength("claude-pc", font=F_TITLE)

    if title:
        badge_x = x + 40 + tw + 8
        draw_badge(draw, badge_x, label_y + 3, 10, badge_color, badge_shape)
        draw.text((badge_x + 16, label_y), title, font=F_TITLE, fill=CARD_TITLE)

    line_y = y + 40
    draw.line([x + 12, line_y, x + w - 12, line_y], fill=CARD_BORDER)

    body_y = line_y + 10
    for line in body_lines:
        cx = x + 15
        if isinstance(line, str):
            line = [(line, CARD_TEXT)]
        for text, color in line:
            draw.text((cx, body_y), text, font=F_TEXT, fill=color)
            cx += draw.textlength(text, font=F_TEXT)
        body_y += 20

    if buttons:
        btn_y = body_y + 10
        btn_h = 30
        cx = x + 15
        gap = 10
        for btext in buttons:
            tw = draw.textlength(btext, font=F_BTN)
            bw = int(tw) + 24
            draw.rounded_rectangle([cx, btn_y, cx + bw, btn_y + btn_h],
                                    radius=6, fill=BUTTON_BG, outline=BUTTON_BORDER, width=1)
            draw.text((cx + 12, btn_y + 8), btext, font=F_BTN, fill=BUTTON_TEXT)
            cx += bw + gap

    return y + h


def draw_user_bubble(draw, x, y, text, is_command=False):
    pad_x, pad_y = 14, 10
    font = F_MONO if is_command else F_TEXT
    lines = text.split("\n")
    tw = max(draw.textlength(l, font=font) for l in lines)
    w = int(tw) + pad_x * 2
    h = len(lines) * 18 + pad_y * 2
    bx = x - w
    draw.rounded_rectangle([bx, y, bx + w, y + h], radius=12, fill=USER_BUBBLE)
    ty = y + pad_y
    for line in lines:
        draw.text((bx + pad_x, ty), line, font=font, fill=USER_TEXT)
        ty += 18
    return y + h


ZONE_CONSOLE = (198, 168, 60)    # 主会话 = 控制台 = 黄
ZONE_TOPIC   = (80, 150, 80)     # 话题 = claude 会话 = 绿


def new_frame(title, in_topic=False, topic_label=None, zone_label=None):
    img = Image.new("RGB", (FRAME_W, FRAME_H), BG_CHAT)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, SIDEBAR_W, FRAME_H], fill=BG_SIDEBAR)
    d.text((12, 14), "消息", font=F_TITLE, fill=(230, 230, 230))
    d.rounded_rectangle([6, 40, SIDEBAR_W - 6, 66], radius=6, fill=(60, 60, 60))
    d.text((14, 46), "claude-pc", font=F_TEXT, fill=(255, 255, 255))
    d.line([SIDEBAR_W + 10, 44, FRAME_W - 10, 44], fill=(230, 230, 230))
    d.text((SIDEBAR_W + 16, 18), "claude-pc", font=F_TITLE_B, fill=(30, 30, 30))
    d.text((SIDEBAR_W + 105, 22), "机器人", font=F_SMALL, fill=(180, 120, 220))
    d.text((FRAME_W - 320, 18), f"帧 · {title}", font=F_SMALL, fill=(150, 150, 150))

    zone_y = 46
    if in_topic:
        color = ZONE_TOPIC
        label = zone_label or f"话题 · session {topic_label or '...'}"
        # 绿底 badge
        d.rounded_rectangle([SIDEBAR_W + 16, zone_y - 2, SIDEBAR_W + 16 + 60, zone_y + 16],
                             radius=3, fill=color)
        d.text((SIDEBAR_W + 22, zone_y), "话题内", font=F_SMALL, fill=(255, 255, 255))
        d.text((SIDEBAR_W + 82, zone_y), label, font=F_SMALL, fill=(60, 60, 60))
    else:
        color = ZONE_CONSOLE
        label = zone_label or "主会话 · 会话管理控制台"
        d.rounded_rectangle([SIDEBAR_W + 16, zone_y - 2, SIDEBAR_W + 16 + 60, zone_y + 16],
                             radius=3, fill=color)
        d.text((SIDEBAR_W + 22, zone_y), "主会话", font=F_SMALL, fill=(255, 255, 255))
        d.text((SIDEBAR_W + 82, zone_y), label, font=F_SMALL, fill=(60, 60, 60))
    return img, d


def frame_1():
    """主会话 · 发普通消息 → 提示控制台,带最近目录按钮(5 个)+ 快捷新会话。"""
    img, d = new_frame("1 · 主会话发普通消息 → 提示 + 最近目录")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "分析一下这个目录")
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "主会话是控制台",
        [
            [("这里是会话管理控制台,不启动 claude。", CARD_TEXT)],
            [("", CARD_TEXT)],
            [("最近目录(点击=用该 cwd 开新话题):", (120, 120, 120))],
        ],
        buttons=["🆕 api (最近)", "🆕 work", "🆕 root", "🆕 docs", "🆕 cache"],
        badge_shape="bar", badge_color=YELLOW)
    return img


def frame_2():
    """主会话 · /new D:\\workcode\\api 开新话题,话题根卡片带历史 session 按钮(5 个)+ 新 session。"""
    img, d = new_frame("2 · 主会话 /new D:\\workcode\\api")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "/new D:\\workcode\\api", is_command=True)
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新话题已开启 · 选一个 session",
        [
            [("cwd: ", CARD_DIM), ("D:\\workcode\\api", ACCENT),
             ("   (话题创建后不可改)", (140, 140, 140))],
            [("", CARD_TEXT)],
            [("点击历史 session 续它,或点 [🆕 新 session] 从头开始:", (120, 120, 120))],
        ],
        buttons=["▶ 3b2526fd", "▶ 3b81c0a4", "▶ 3be7f2a1", "▶ 4c1f9b82", "▶ 5a2d4c1e", "🆕 新"],
        badge_shape="circle", badge_color=GREEN)
    return img


def frame_3():
    """话题内 · 点击 [🆕 新 session] → 从零开始的新 session。"""
    img, d = new_frame("3 · 点 [新 session] 按钮", in_topic=True,
                       topic_label="(未开始)")
    y = 68
    # 帧 2 的卡片复述(简版)—— 让用户看到点击了哪个按钮
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新话题 · 选 session",
        [
            [("cwd: ", CARD_DIM), ("D:\\workcode\\api", ACCENT)],
        ],
        buttons=["▶ 3b2526fd", "▶ 3b81c0a4", "▶ 3be7f2a1", "▶ 4c1f9b82", "▶ 5a2d4c1e", "🆕 新"],
        badge_shape="circle", badge_color=GREEN)
    y += 12
    d.text((SIDEBAR_W + 22, y), "↑ 用户点击 [🆕 新] 按钮", font=F_SMALL, fill=(150, 90, 200))
    y += 26
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新 session 已开启",
        [
            [("▶ session: ", CARD_TEXT), ("a91f3c2e...", ACCENT),
             ("   (新,无 --resume)", CARD_DIM)],
            [("   cwd:   ", CARD_TEXT), ("D:\\workcode\\api", ACCENT)],
            [("", CARD_TEXT)],
            [("发普通消息即启动 claude。", CARD_TEXT)],
        ])
    return img


def frame_4():
    """话题内 · 点击历史 session 按钮 → 直接续那个 session。"""
    img, d = new_frame("4 · 点击历史 session 按钮", in_topic=True,
                       topic_label="(未开始)")
    y = 68
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新话题 · 选 session",
        [
            [("cwd: ", CARD_DIM), ("D:\\workcode\\api", ACCENT)],
        ],
        buttons=["▶ 3b2526fd", "▶ 3b81c0a4", "▶ 3be7f2a1", "▶ 4c1f9b82", "▶ 5a2d4c1e", "🆕 新"],
        badge_shape="circle", badge_color=GREEN)
    y += 12
    d.text((SIDEBAR_W + 22, y), "↑ 用户点击 [▶ 3b81c0a4] 按钮", font=F_SMALL, fill=(150, 90, 200))
    y += 26
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "已选续 session",
        [
            [("▶ session: ", CARD_TEXT), ("3b81c0a4...", ACCENT),
             ("   (将 --resume)", CARD_DIM)],
            [("   之前: 7 轮,上次活动 昨天", CARD_TEXT)],
            [("", CARD_TEXT)],
            [("发普通消息即 resume 该 session。", CARD_TEXT)],
        ])
    return img


def frame_5():
    """话题内 · 普通消息进 claude,得到 claude 回复。无按钮。"""
    img, d = new_frame("5 · 话题内发消息 → claude", in_topic=True,
                       topic_label="session 3b81c0a4...")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "分析这个目录")
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "回复",
        [
            [("在 D:\\workcode\\api 下:", CARD_TEXT)],
            [("(略 — 这里是 Claude 的实际回复)", CARD_DIM)],
        ])
    return img


def frame_6():
    """话题内 · 多轮对话(claude 是常驻进程,session 连续)。"""
    img, d = new_frame("6 · 话题内多轮对话", in_topic=True,
                       topic_label="session 3b81c0a4...")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "把 README 翻译成英文")
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "回复",
        [
            [("已完成。改动 3 个文件:", CARD_TEXT)],
            [("   README.md / README.en.md / docs/quickstart.md", CARD_DIM)],
            [("", CARD_TEXT)],
            [("继续发消息会保持这个上下文。", CARD_DIM)],
        ])
    return img


def frame_7():
    """话题内 · 想换 cwd → 回主会话 /new(话题内无 /cd)。"""
    img, d = new_frame("7 · 想换 cwd → 回主会话 /new")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "/new D:\\workcode\\work", is_command=True)
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新话题已开启",
        [
            [("cwd: ", CARD_DIM), ("D:\\workcode\\work", ACCENT)],
            [("", CARD_TEXT)],
            [("旧话题(3b81c0a4)不受影响,仍在飞书左侧列表。", CARD_TEXT)],
        ],
        buttons=["▶ 1ea68510", "▶ 2f90b1c4", "▶ 3a91d2e5", "▶ 4e2c8b17", "▶ 5c1f9a03", "🆕 新"],
        badge_shape="circle", badge_color=GREEN)
    return img


def frame_8():
    """主会话 · /new 无参 = 用最近目录开新话题。"""
    img, d = new_frame("8 · 主会话 /new (无参=最近目录)")
    y = 68
    y = draw_user_bubble(d, FRAME_W - 20, y, "/new", is_command=True)
    y += 22
    y = draw_bot_card(d, SIDEBAR_W + 20, y, FRAME_W - SIDEBAR_W - 40, "新话题已开启",
        [
            [("cwd: ", CARD_DIM), ("D:\\workcode\\api", ACCENT),
             ("   (最近的目录)", (140, 140, 140))],
        ],
        buttons=["▶ 3b2526fd", "▶ 3b81c0a4", "▶ 3be7f2a1", "▶ 4c1f9b82", "▶ 5a2d4c1e", "🆕 新"],
        badge_shape="circle", badge_color=GREEN)
    return img


def main():
    frames = [frame_1, frame_2, frame_3, frame_4, frame_5, frame_6, frame_7, frame_8]
    pad = 20
    gap = 14
    W = FRAME_W + pad * 2
    H = pad * 2 + sum(FRAME_H for _ in frames) + gap * (len(frames) - 1)
    canvas = Image.new("RGB", (W, H), (240, 240, 242))
    d = ImageDraw.Draw(canvas)

    d.text((pad, 8), "feishu-bridge-go · 交互重设计 · 8 帧", font=F_TITLE_B, fill=(20, 20, 20))
    d.text((pad, 30),
           "主会话:按钮=最近 5 个目录(每个点=用该 cwd 开新话题)。/new 无参=用最近目录。",
           font=F_SMALL, fill=(80, 80, 80))
    d.text((pad, 46),
           "话题创建:按钮=最近 5 个 session(每个点=续它)+ [🆕 新]。话题内回复:只有 claude 输出,无按钮。cwd 定死,无 /cd /resume。",
           font=F_SMALL, fill=(80, 80, 80))

    y = pad + 66
    for fn in frames:
        img = fn()
        canvas.paste(img, (pad, y))
        y += FRAME_H + gap

    canvas.save(OUT, "PNG", optimize=True)
    print(f"wrote {OUT}  ({W}x{H})")


if __name__ == "__main__":
    main()
