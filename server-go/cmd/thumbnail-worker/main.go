// thumbnail-worker は FFmpeg のデコード状態を API サーバーのプロセスから分離する。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"

	"github.com/aki0429/KonomiTV/server-go/internal/frameextract"
)

var buildRevision = "development"

func main() {
	version := flag.Bool("version", false, "print build revision")
	flag.Parse()
	if *version {
		fmt.Println("KonomiTV thumbnail worker", buildRevision)
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var request frameextract.Request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 4<<20)).Decode(&request); err != nil {
		return fmt.Errorf("decode thumbnail request: %w", err)
	}
	if len(request.Offsets) == 0 || len(request.Offsets) > 100000 || request.OutputDirectory == "" {
		return fmt.Errorf("invalid thumbnail request")
	}
	library, err := frameextract.Load(request.LibraryDirectory)
	if err != nil {
		return err
	}
	result, err := library.Extract(context.Background(), request, func(index int, rgb *frameextract.RGB) error {
		img := image.NewNRGBA(image.Rect(0, 0, rgb.Width, rgb.Height))
		for i := range rgb.Width * rgb.Height {
			copy(img.Pix[i*4:i*4+3], rgb.Pixels[i*3:i*3+3])
			img.Pix[i*4+3] = 255
		}
		path := filepath.Join(request.OutputDirectory, fmt.Sprintf("frame_%05d.png", index+1))
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		encoder := png.Encoder{CompressionLevel: png.BestSpeed}
		encodeErr := encoder.Encode(file, img)
		closeErr := file.Close()
		if encodeErr != nil {
			return encodeErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
