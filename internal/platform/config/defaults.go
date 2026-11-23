package config

import "time"

func dur(d time.Duration) Duration { return Duration(d) }

// Defaults returns the configuration used for every key missing from config.yaml.
func Defaults() *Config {
	return &Config{
		Twitch: Twitch{
			LivePollInterval: dur(45 * time.Second),
			CaptureQuality:   "720p60",
			TokenFile:        "/data/twitch_user_token.json",
			Games:            []string{"Grand Theft Auto VI"},
		},
		Capture: Capture{
			BufferDir:       "/buffer",
			PendingDir:      "/pending",
			BufferDuration:  dur(10 * time.Minute),
			BufferMaxBytes:  4 << 30,
			SegmentDuration: dur(2 * time.Second),
		},
		Approval: Approval{TTL: dur(6 * time.Hour)},
		Concurrency: Concurrency{
			MaxStreams:          8,
			FFmpegWorkers:       4,
			FFmpegThreadsPerJob: 4,
			TranscribeWorkers:   1,
			RenderWorkers:       1,
		},
		Render: Render{
			OutputDir:        "/output",
			Width:            1080,
			Height:           1920,
			FPS:              30,
			Encoder:          "h264_nvenc",
			MinDuration:      dur(15 * time.Second),
			MaxDuration:      dur(60 * time.Second),
			MaxFileBytes:     45 << 20,
			PreviewHeight:    854,
			PreviewDuration:  dur(40 * time.Second),
			PreviewBefore:    dur(25 * time.Second),
			Font:             "Noto Sans",
			MaxLineChars:     28,
			HeadlineMaxChars: 60,
		},
		Whisper: Whisper{URL: "http://whisper:8080", Timeout: dur(5 * time.Minute), BeamSize: 5, GameLanguage: "en"},
		Editor: Editor{
			// Thinking tokens count toward max_tokens on current models; only
			// the tokens actually generated are paid.
			MaxTokens:         16000,
			Timeout:           dur(90 * time.Second),
			MaxCostPerClipUSD: 0.05,
			ToolChoice:        "tool",
			PeakTolerance:     dur(5 * time.Second),
			FrameInterval:     dur(3 * time.Second),
			MaxFrames:         40,
		},
		YouTube: YouTube{
			TokenFile:         "/data/youtube_token.json",
			Privacy:           "public",
			CategoryID:        "20", // Gaming
			NotifySubscribers: true,
			UploadTimeout:     dur(15 * time.Minute),
			TitleHashtags:     []string{"#GTA6", "#Shorts"},
			Hashtags:          []string{"#GTA6", "#GTAVI", "#GrandTheftAutoVI", "#Shorts", "#{streamer}"},
			// Most important first: the 500 character budget cuts the tail
			// (the editor's tags of the moment come before these).
			Tags: []string{"{streamer}", "{streamer} gta 6", "GTA 6", "GTA VI", "GTA6", "Grand Theft Auto VI",
				"Grand Theft Auto 6", "GTA 6 gameplay", "GTA 6 clips", "GTA 6 shorts", "GTA 6 momentos engraçados",
				"GTA 6 funny moments", "GTA 6 Brasil", "GTA 6 PT-BR", "GTA 6 live", "Vice City", "Leonida",
				"Rockstar Games", "clipes da twitch", "twitch clips", "melhores momentos"},
		},
		HTTP:   HTTP{Addr: ":8080", ShutdownTimeout: dur(45 * time.Second)},
		DBPath: "/data/clipper.db",
		Detection: Detection{
			Tick:         dur(500 * time.Millisecond),
			Warmup:       dur(5 * time.Minute),
			Cooldown:     dur(60 * time.Second),
			ThresholdOn:  4.0,
			ThresholdOff: 1.5,
			MinSignalZ:   3.5,
			ZMax:         6.0,
			PreRoll:      dur(60 * time.Second),
			PostRoll:     dur(30 * time.Second),
			Weights:      map[string]float64{"voice": 0.45, "chat": 0.45, "clips": 0.10},
			Voice: VoiceDetection{
				Frame:      dur(30 * time.Millisecond),
				Window:     dur(400 * time.Millisecond),
				EWMAAlpha:  0.01,
				StaleAfter: dur(2 * time.Second),
			},
			Chat: ChatDetection{
				Window:    dur(10 * time.Second),
				Sustain:   dur(4 * time.Second),
				EWMAAlpha: 0.02,
				Emotes: map[string]map[string]float64{
					"en": {"KEKW": 2.0, "LUL": 1.5, "OMEGALUL": 2.5, "LMAO": 1.5},
					"pt": {"kkkk": 2.0, "KKKK": 2.0},
				},
			},
			Clips: ClipsDetection{
				PollInterval: dur(30 * time.Second),
				GroupWindow:  dur(15 * time.Second),
				MinClips:     3,
				MinZ:         4.0,
				Lookback:     dur(45 * time.Second),
			},
			Calibration: Calibration{Window: 20, StepUp: 0.25, StepDown: 0.25, MaxOffset: 1.5},
		},
	}
}
