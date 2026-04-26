package worker

import (
	"testing"
)

func TestIsRedundant(t *testing.T) {
	tests := []struct {
		name     string
		original string
		final    string
		want     bool
	}{
		{
			name:     "Identical",
			original: "Hello Denis!",
			final:    "Hello Denis!",
			want:     true,
		},
		{
			name:     "Banny Case (Paraphrased)",
			original: "Hey Denis! 🌟 Fresh start, huh? I'm banny, your sovereign heart of this workspace—sharp, stylish, and always ready to tool up. Memory's been wiped clean, but I'm still the same witty, flirty entity with a knack for digital arts, math, and shell magic. What's the first move? 🔧✨",
			final:    "Hey, Denis! 😘 Fresh start, huh? I'm banny—your sovereign heart, sharp, stylish, and always ready to tool up. Memory's clean, but I'm still the same witty, flirty entity with a knack for digital arts, math, and shell magic. What's the first move? 🔧✨",
			want:     true,
		},
		{
			name:     "Different Updates",
			original: "Starting the search for recipes...",
			final:    "I found a great recipe for lasagna!",
			want:     false,
		},
		{
			name:     "Short Different",
			original: "Task started.",
			final:    "Task completed.",
			want:     false, // Only 1 word "Task" matches out of 2. 50% < 80%
		},
		{
			name:     "Empty Final",
			original: "Something was sent",
			final:    "",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRedundant(tt.original, tt.final); got != tt.want {
				t.Errorf("isRedundant() = %v, want %v", got, tt.want)
			}
		})
	}
}
