"""Streaming speech-to-text over WebSocket: zipformer partials, SenseVoice final.

Protocol (one utterance per connection):

  client -> server   binary frames of PCM16LE, 16 kHz, mono (any chunk size)
  client -> server   text frame {"type": "end"}
  server -> client   {"partial": "...", "t": 1.2}
  server -> client   {"final": "...", "stream": "...", "elapsed": 4.1}
  server -> client   {"error": "..."}

Partials come from a streaming zipformer (incremental, ~ms per chunk); the
final is re-decoded from the whole buffer with SenseVoice, which is more
accurate but cannot decode incrementally.
"""

import asyncio
import json
import os

import numpy as np
import sherpa_onnx
import websockets

SENSE = os.environ.get(
    "ASR_MODEL",
    r"C:\Users\yibo\tools\models\sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17\model.int8.onnx",
)
SENSE_TOKENS = os.environ.get(
    "ASR_TOKENS",
    r"C:\Users\yibo\tools\models\sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2024-07-17\tokens.txt",
)
STREAM_DIR = os.environ.get(
    "ASR_STREAM_MODEL",
    r"C:\Users\yibo\tools\models\sherpa-onnx-streaming-zipformer-multi-zh-hans-int8-2023-12-13",
)
PORT = int(os.environ.get("ASR_STREAM_PORT", "8711"))

sense = sherpa_onnx.OfflineRecognizer.from_sense_voice(
    model=SENSE, tokens=SENSE_TOKENS, use_itn=True, num_threads=2, debug=False
)

online = sherpa_onnx.OnlineRecognizer.from_transducer(
    tokens=os.path.join(STREAM_DIR, "tokens.txt"),
    encoder=os.path.join(STREAM_DIR, "encoder-epoch-20-avg-1-chunk-16-left-128.int8.onnx"),
    decoder=os.path.join(STREAM_DIR, "decoder-epoch-20-avg-1-chunk-16-left-128.onnx"),
    joiner=os.path.join(STREAM_DIR, "joiner-epoch-20-avg-1-chunk-16-left-128.int8.onnx"),
    num_threads=2,
    sample_rate=16000,
    feature_dim=80,
    decoding_method="greedy_search",
    enable_endpoint_detection=False,
)


def stream_text(st):
    r = online.get_result(st)
    return (r.text if hasattr(r, "text") else str(r)).strip()


def sense_text(samples):
    s = sense.create_stream()
    s.accept_waveform(16000, samples)
    sense.decode_stream(s)
    return s.result.text.strip()


async def handle(ws):
    started = asyncio.get_event_loop().time()
    st = online.create_stream()
    chunks = []            # every sample, for the SenseVoice final
    pending = b""          # audio not yet fed to the streaming decoder
    last = ""
    last_sent = 0.0
    try:
        async for message in ws:
            if isinstance(message, bytes):
                pending += message
                # feed in 100 ms blocks: the decoder's per-call overhead is
                # what makes 32 ms frames fall behind real time
                while len(pending) >= 3200:
                    block, pending = pending[:3200], pending[3200:]
                    pcm = np.frombuffer(block, dtype="<i2").astype(np.float32) / 32768.0
                    chunks.append(pcm)
                    st.accept_waveform(16000, pcm)
                    changed = False
                    while online.is_ready(st):
                        online.decode_stream(st)
                        changed = True
                    text = stream_text(st) if changed else last
                    now = round((len(chunks) * 3200) / 2 / 16000.0, 2)
                    if text and text != last and now - last_sent >= 0.1:
                        last, last_sent = text, now
                        await ws.send(json.dumps(
                            {"partial": text, "t": now,
                             "wall": round(asyncio.get_event_loop().time() - started, 2)},
                            ensure_ascii=False))
            else:
                if json.loads(message).get("type") == "end":
                    break
        if pending:
            pcm = np.frombuffer(pending[:len(pending)//2*2], dtype="<i2").astype(np.float32) / 32768.0
            chunks.append(pcm)
            st.accept_waveform(16000, pcm)
            while online.is_ready(st):
                online.decode_stream(st)
        buf = np.concatenate(chunks) if chunks else np.zeros(0, dtype=np.float32)
        streamed = stream_text(st) if buf.size else ""
        final = ""
        if buf.size:
            final = await asyncio.to_thread(sense_text, buf)
        await ws.send(json.dumps(
            {"final": final or streamed, "stream": streamed,
             "elapsed": round(asyncio.get_event_loop().time() - started, 2)},
            ensure_ascii=False))
    except websockets.ConnectionClosed:
        pass
    except Exception as e:
        try:
            await ws.send(json.dumps({"error": str(e)}, ensure_ascii=False))
        except Exception:
            pass


async def main():
    print(f"stream asr on :{PORT} (zipformer partials + sensevoice final)", flush=True)
    async with websockets.serve(handle, "0.0.0.0", PORT, max_size=None):
        await asyncio.Future()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
