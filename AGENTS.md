# Project rules

## Never play audio on the device (user mandate, 2026-11-11)

The CardputerZero is used in public places, so **no test may unmute the
speaker or play sound through it**.

- Leave the sink muted: `wpctl set-mute @DEFAULT_AUDIO_SINK@ 1` (plus
  `amixer -c 1 sset Speaker mute` for the ES8389).
- Test voice input with a file, not with sound in the room:
  `czgorex -fakewav sample.wav -stt-cloud …`, or `-voicetest` against a
  recorded file.
- `aplay` / `pw-play` / any playback is off limits unless the user asks for it
  in that same request.
- After any experiment that touches the mixer, re-mute and verify:
  `wpctl get-volume @DEFAULT_AUDIO_SINK@` must read `[MUTED]`.

## Recording is fine

Microphone capture is allowed — it is silent. Recordings stay in
`~/.local/share/czgorex/voice-*.wav`.
