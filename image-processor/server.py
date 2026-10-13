"""Internal CPU image processor; no account credentials or state."""
import io
import os
from pathlib import Path
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

from PIL import Image, ImageOps

ROOT = Path(__file__).resolve().parent
LIMIT = 64 * 1024 * 1024
PIXELS = 32 * 1024 * 1024
Image.MAX_IMAGE_PIXELS = PIXELS
SLOTS = threading.BoundedSemaphore(max(1, min(8, int(os.getenv("IMAGE_PROCESSOR_CONCURRENCY", "2")))))
MODELS = threading.local()


def process(raw, width, height, engine):
    if not 1 <= width <= 8192 or not 1 <= height <= 8192 or width * height > PIXELS:
        raise ValueError("invalid dimensions")
    with Image.open(io.BytesIO(raw)) as source:
        if source.width * source.height > PIXELS:
            raise ValueError("input exceeds pixel limit")
        if source.width >= width and source.height >= height:
            return raw, Image.MIME.get(source.format, "image/png")
        image = ImageOps.exif_transpose(source)
        fmt = source.format if source.format in {"PNG", "JPEG", "WEBP"} else "PNG"
        if engine == "sharp_lanczos3":
            try:
                result = subprocess.run(
                    ["node", str(ROOT / "sharp/upscale.mjs"), str(width), str(height)],
                    input=raw, capture_output=True, timeout=160, check=True,
                ).stdout
                if not result or len(result) > LIMIT:
                    raise ValueError("invalid Sharp output")
                return result, Image.MIME[fmt]
            except (OSError, subprocess.SubprocessError):
                engine = "pillow_lanczos"
        if engine == "pillow_lanczos":
            image = image.resize((width, height), Image.Resampling.LANCZOS, reducing_gap=3.0)
        elif engine == "fsrcnn_x2":
            import cv2
            import numpy as np
            if image.width * image.height * 4 > PIXELS:
                raise ValueError("FSRCNN intermediate exceeds pixel limit")
            cv2.setNumThreads(1)
            if not hasattr(MODELS, "fsrcnn"):
                MODELS.fsrcnn = cv2.dnn_superres.DnnSuperResImpl_create()
                MODELS.fsrcnn.readModel(str(ROOT / "models/FSRCNN_x2.pb"))
                MODELS.fsrcnn.setModel("fsrcnn", 2)
            rgb = cv2.cvtColor(np.asarray(image.convert("RGB")), cv2.COLOR_RGB2BGR)
            out = MODELS.fsrcnn.upsample(rgb)
            out = cv2.resize(out, (width, height), interpolation=cv2.INTER_LANCZOS4)
            image_out = Image.fromarray(cv2.cvtColor(out, cv2.COLOR_BGR2RGB))
            if "A" in image.getbands():
                image_out.putalpha(image.getchannel("A").resize((width, height), Image.Resampling.BICUBIC))
            image = image_out
        else:
            raise ValueError("unsupported engine")
        if fmt == "JPEG" and image.mode not in {"RGB", "L"}:
            image = image.convert("RGB")
        buf = io.BytesIO()
        options = {"quality": 95, "subsampling": 0} if fmt == "JPEG" else {"quality": 95, "method": 4} if fmt == "WEBP" else {}
        image.save(buf, format=fmt, **options)
        data = buf.getvalue()
        if len(data) > LIMIT:
            raise ValueError("output exceeds limit")
        return data, Image.MIME[fmt]


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(180)

    def reply(self, status, data, mime="text/plain"):
        self.send_response(status)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(200 if self.path == "/health" else 404, b"ok")

    def do_POST(self):
        if urlsplit(self.path).path != "/upscale":
            self.reply(404, b"not found")
            return
        if not SLOTS.acquire(timeout=5):
            self.reply(503, b"processor busy")
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= LIMIT:
                self.reply(413, b"invalid input size")
                return
            raw = self.rfile.read(size)
            if len(raw) != size:
                raise ValueError("truncated image")
            query = parse_qs(urlsplit(self.path).query)
            width, height = map(int, query["size"][0].split("x"))
            data, mime = process(raw, width, height, query["engine"][0])
            self.reply(200, data, mime)
        except (ValueError, KeyError, OSError, Image.DecompressionBombError):
            self.reply(400, b"invalid image processing request")
        except Exception:
            self.reply(502, b"image processing failed")
        finally:
            SLOTS.release()

    def log_message(self, *_):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
