package render

import (
	"fmt"
	"strconv"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
	"github.com/go-fonts/latin-modern/lmsans10bold"
	"github.com/go-fonts/latin-modern/lmsans10regular"
	"github.com/tdewolff/canvas"
)

// Fonts resolves theme font families from the config (TTF file paths) with a
// fallback to the embedded Latin Modern fonts, so themes render identically
// on machines without system fonts installed.
type Fonts struct {
	files   map[string]config.FontFiles
	loaded  map[string]*canvas.Font
	defReg  *canvas.Font
	defBold *canvas.Font
}

// NewFonts wraps the config font map (may be nil).
func NewFonts(files map[string]config.FontFiles) *Fonts {
	return &Fonts{files: files, loaded: make(map[string]*canvas.Font)}
}

// get loads (and caches) the font for a family/bold combination.
func (f *Fonts) get(family string, bold bool) (*canvas.Font, error) {
	if f.loaded == nil {
		f.loaded = make(map[string]*canvas.Font)
	}
	key := family + "\x00" + strconv.FormatBool(bold)
	if ft, ok := f.loaded[key]; ok {
		return ft, nil
	}
	style := canvas.FontRegular
	if bold {
		style = canvas.FontBold
	}
	if ff, ok := f.files[family]; ok {
		path := ff.Regular
		if bold && ff.Bold != "" {
			path = ff.Bold
		}
		if path != "" {
			ft, err := canvas.LoadFontFile(path, style)
			if err != nil {
				return nil, fmt.Errorf("render: load font %q: %w", path, err)
			}
			f.loaded[key] = ft
			return ft, nil
		}
	}
	// embedded fallback
	if bold {
		if f.defBold == nil {
			ft, err := canvas.LoadFont(lmsans10bold.TTF, 0, canvas.FontBold)
			if err != nil {
				return nil, fmt.Errorf("render: embedded fallback: %w", err)
			}
			f.defBold = ft
		}
		f.loaded[key] = f.defBold
		return f.defBold, nil
	}
	if f.defReg == nil {
		ft, err := canvas.LoadFont(lmsans10regular.TTF, 0, canvas.FontRegular)
		if err != nil {
			return nil, fmt.Errorf("render: embedded fallback: %w", err)
		}
		f.defReg = ft
	}
	f.loaded[key] = f.defReg
	return f.defReg, nil
}
