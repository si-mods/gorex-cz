"""Tiny LAN speech-to-text service: POST a WAV body, get text back.

Wraps sherpa-onnx's offline CLI with the SenseVoice int8 model, so the
CardputerZero can use it as its network STT engine:

    czgorex -stt-cloud 'curl -s --data-binary @$VOICE_WAV http://<pc>:8710/asr'
"""

import http.server
import json
import os
import subprocess
import tempfile
import threading

BIN = r"C:\Users\yibo\tools\sherpa\sherpa-onnx-v2.0.1-win-x64-shared-MD-MinSizeRel-no-tts\bin\sherpa-onnx-offline.exe"
MODEL = r"C:\Users\yibo\tools\models\sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17\model.int8.onnx"
TOKENS = r"C:\Users\yibo\tools\models\sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17\tokens.txt"
PORT = int(os.environ.get("ASR_PORT", "8710"))

lock = threading.Lock()


def transcribe(path):
    cmd = [
        BIN,
        f"--sense-voice-model={MODEL}",
        f"--tokens={TOKENS}",
        "--sense-voice-use-itn=1",
        "--num-threads=2",
        path,
    ]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=120).stdout
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("{"):
            return json.loads(line).get("text", "")
    return ""


def multipart_wav(body, ctype):
    """Pull the `file` part out of a multipart/form-data body."""
    try:
        boundary = ctype.split("boundary=", 1)[1].strip().strip('"')
    except IndexError:
        return None
    for part in body.split(b"--" + boundary.encode()):
        if b'name="file"' in part:
            head, _, data = part.partition(b"\r\n\r\n")
            return data.rstrip(b"\r\n-")
    return None


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        data = self.rfile.read(n)
        ctype = self.headers.get("Content-Type", "")
        json_out = self.path.startswith("/v1/audio/transcriptions")
        if "multipart/form-data" in ctype:  # OpenAI-compatible shape
            wav = multipart_wav(data, ctype)
            if wav is None:
                self.send_error(400, "no file part")
                return
            data = wav
            json_out = True
        with lock, tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
            f.write(data)
            path = f.name
        try:
            text = transcribe(path)
        except Exception as e:  # report, do not crash
            text = f"[asr error: {e}]"
        finally:
            os.unlink(path)
        qs = self.path.split("?", 1)[1] if "?" in self.path else ""
        fmt = ""
        for kv in qs.split("&"):
            if kv.startswith("response_format="):
                fmt = kv.split("=", 1)[1]
        if fmt == "text":
            json_out = False
        if json_out or fmt == "json":
            body = json.dumps({"text": text}, ensure_ascii=False).encode("utf-8")
            ctype_out = "application/json; charset=utf-8"
        else:
            body = text.encode("utf-8")
            ctype_out = "text/plain; charset=utf-8"
        self.send_response(200)
        self.send_header("Content-Type", ctype_out)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print(f"sherpa sensevoice asr on :{PORT}", flush=True)
    http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
