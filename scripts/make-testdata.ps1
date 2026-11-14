<#
.SYNOPSIS
  Regenerates the speech samples in testdata/ using the Windows TTS (System.Speech).

.DESCRIPTION
  The TTS engine reports the exact audio position where each word starts
  (SpeakProgress.AudioPosition), which is the ground truth used to measure the
  precision of whisper.cpp word timestamps (docs/decisions/0012).
  Outputs 16 kHz mono WAV files plus <name>.words.json with {text,start_ms}.
  Also builds testdata/video/sample.mp4 (needs ffmpeg on PATH).
#>
[CmdletBinding()]
param(
  [string]$Root = (Split-Path -Parent $PSScriptRoot)
)
$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.Speech
Add-Type -ReferencedAssemblies System.Speech, System.Core -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Speech.Synthesis;
using System.Speech.AudioFormat;

public static class SpeechSample {
    public static string Make(string voice, string text, string wavPath, int rate) {
        var words = new List<string>();
        using (var s = new SpeechSynthesizer()) {
            s.SelectVoice(voice);
            s.Rate = rate;
            s.SetOutputToWaveFile(wavPath, new SpeechAudioFormatInfo(16000, AudioBitsPerSample.Sixteen, AudioChannel.Mono));
            s.SpeakProgress += (o, e) => {
                var t = e.Text.Replace("\\", "\\\\").Replace("\"", "\\\"");
                words.Add(string.Format(System.Globalization.CultureInfo.InvariantCulture,
                    "{{\"text\":\"{0}\",\"start_ms\":{1}}}", t, (long)e.AudioPosition.TotalMilliseconds));
            };
            s.Speak(text);
            s.SetOutputToNull();
        }
        return "[" + string.Join(",", words) + "]";
    }
}
'@

$speechDir = Join-Path $Root 'testdata/speech'
$videoDir = Join-Path $Root 'testdata/video'
New-Item -ItemType Directory -Force $speechDir, $videoDir | Out-Null

$samples = @(
  @{ Name = 'en_sample'; Voice = 'Microsoft Zira Desktop'; Rate = 0; Text = 'No way, did you see that? He jumped off the bridge and landed right on the police car. That is the craziest thing I have seen all week. Chat, clip it, clip it right now. Okay, okay, we need to get out of here before the helicopter shows up.' },
  @{ Name = 'pt_sample'; Voice = 'Microsoft Maria Desktop'; Rate = 0; Text = 'Gente, vocês viram isso? Ele pulou da ponte e caiu bem em cima da viatura. Foi a coisa mais absurda que eu vi essa semana. Clipa isso agora, chat. Bora sair daqui antes que o helicóptero chegue.' }
)

foreach ($s in $samples) {
  $wav = Join-Path $speechDir ($s.Name + '.wav')
  $json = [SpeechSample]::Make($s.Voice, $s.Text, $wav, $s.Rate)
  # UTF-8 without BOM, LF.
  [System.IO.File]::WriteAllText((Join-Path $speechDir ($s.Name + '.words.json')), $json + "`n", (New-Object System.Text.UTF8Encoding $false))
  Write-Host "wrote $wav"
}

# 40 s 640x360 test video: 8 s of quiet game-like noise, the English speech, then noise again.
$en = Join-Path $speechDir 'en_sample.wav'
$out = Join-Path $videoDir 'sample.mp4'
& ffmpeg -hide_banner -loglevel error -y `
  -f lavfi -i 'testsrc2=size=640x360:rate=30' `
  -f lavfi -i 'anoisesrc=color=pink:amplitude=0.02:sample_rate=48000' `
  -i $en `
  -filter_complex '[2:a]aresample=48000,adelay=8000|8000,apad[sp];[1:a][sp]amix=inputs=2:duration=first:normalize=0[a]' `
  -map 0:v -map '[a]' -t 40 `
  -c:v libx264 -preset veryfast -crf 32 -g 60 -pix_fmt yuv420p `
  -c:a aac -b:a 64k -ac 1 -movflags +faststart $out
if ($LASTEXITCODE -ne 0) { throw "ffmpeg failed" }
Write-Host "wrote $out"
