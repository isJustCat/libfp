package main

import (
	"bytes"
	_ "embed"
	"errors"
	"github.com/google/uuid"
	"github.com/rileys-trash-can/libfp/prbuf"
	"io"
	"log"
	"sync"
	"time"

	// image stuffs
	"github.com/rileys-trash-can/gorm-sqlite-cgo-free"
	"github.com/samuel/go-pcx/pcx"
	"golang.org/x/image/bmp"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"image"
	"image/jpeg"
	"image/png"
	_ "modernc.org/sqlite"
)

func SaveImage(img image.Image, uuid uuid.UUID, public bool, name string) error {
	b := &bytes.Buffer{}
	err := encodeImage(b, img, "png")
	if err != nil {
		return err
	}

	return saveImage(b.Bytes(), uuid, public, name, "png")
}

func saveImage(b []byte, uuid uuid.UUID, public bool, name, ext string) error {
	GetDB().Create(&Image{
		UUID: uuid,

		Ext:     ext,
		Data:    b,
		Public:  public,
		Name:    name,
		Created: time.Now(),
	})
	return nil
}

func UpdateImage(uuid uuid.UUID, newimg image.Image) error {
	b := &bytes.Buffer{}
	err := encodeImage(b, newimg, "png")
	if err != nil {
		return err
	}

	return updateImage(uuid, b.Bytes())
}

func updateImage(uuid uuid.UUID, b []byte) error {
	img := &Image{UUID: uuid}

	GetDB().Model(&img).Update("Data", b)

	return nil
}

func GetImage(uuid uuid.UUID) (i Image) {
	GetDB().First(&i, uuid)

	return
}

var db *gorm.DB
var dbOnce sync.Once

func openDB() {
	var err error
	conf := GetConfig()

	switch conf.DBType {
	case "sqlite":
	case "sqlite3":
		db, err = gorm.Open(sqlite.Open(conf.DB), &gorm.Config{})
		if err != nil {
			log.Fatalf("Failed to open db: %s", err)
		}

	case "mysql":
		db, err = gorm.Open(mysql.Open(conf.DB), &gorm.Config{})
		if err != nil {
			log.Fatalf("Failed to open db: %s", err)
		}

	default:
		log.Fatalf("Invalid DBType: sqlite or mysql is valid")
	}

	err = db.AutoMigrate(&Image{})
	if err != nil {
		log.Fatalf("Failed to AutoMigrate: %s", err)
	}
}

type Image struct {
	Created time.Time

	UUID        uuid.UUID
	UnProcessed *uuid.UUID // unprocessed counterpart
	Processed   *uuid.UUID // processed counterpart

	IsProcessed bool   //
	Ext         string // ext of image e.g. .png
	Data        []byte
	Public      bool
	Name        string // optionally the origin files name
}

func (i *Image) ProcessedString() string {
	if i.IsProcessed {
		return "processed"
	}

	return "raw"
}

func GetDB() *gorm.DB {
	dbOnce.Do(openDB)

	return db
}

func encodeImage(w io.Writer, img image.Image, fmt string) (err error) {
	if *OptVerbose {
		log.Printf("Encoding image with bounds %v in format %s", img.Bounds().Size(), fmt)
	}

	switch fmt {
	case "png":
		return png.Encode(w, img)

	case "jpg":
	case "jpeg":
		return jpeg.Encode(w, img, &jpeg.Options{
			Quality: 50,
		})

	case "pcx":
		return pcx.Encode(w, img)

	case "bmp":
		return bmp.Encode(w, img)

	case "prbuf":
		prbuf.Encode(img, w)
		return nil
	}

	return errors.New("Unknown Image Format")
}
