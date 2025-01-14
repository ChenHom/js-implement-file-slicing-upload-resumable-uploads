package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
)

func generateImage(filename string, width, height int) {
	// 建立指定大小的圖片
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// 填充圖片背景為白色
	white := color.RGBA{255, 255, 255, 255}
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, white)
		}
	}

	// 在圖片中間畫一個紅色的矩形
	red := color.RGBA{255, 0, 0, 255}
	for x := width / 4; x < 3*width/4; x++ {
		for y := height / 4; y < 3*height/4; y++ {
			img.Set(x, y, red)
		}
	}

	// 儲存圖片到檔案
	file, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	err = png.Encode(file, img)
	if err != nil {
		panic(err)
	}

	println("Image saved to", filename)
}
