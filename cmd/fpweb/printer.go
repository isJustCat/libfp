package main

import (
	"github.com/disintegration/imaging"
	"github.com/rileys-trash-can/libfp"

	_ "embed"
	"github.com/google/uuid"
	"image"
	"image/color"
	"image/draw"
	"log"
	"time"
)

var (
	printQ = make(chan *PrintJob, 10)
)

type PrintJob struct {
	Img  image.Image
	UUID uuid.UUID

	PFCount   uint
	LabelSize image.Point
	ditherer  Filter

	optresize  bool
	optstretch bool
	optrotate  bool
	optcenterh bool
	optcenterv bool
	opttiling  bool
}

func init() {
	go goPrintQ()
}

func goPrintQ() {
	const totalSteps = 8

	for {
		select {
		case job := <-printQ:
			if *OptVerbose {
				log.Printf("Got printjob %+v", job)
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "decode",
				Progress: 1.0 / totalSteps,
				Done:     false,
			}

			//TODO more options
			var method = imaging.Lanczos

			img := job.Img

			size := img.Bounds().Size()

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "rotating",
				Progress: 2.0 / totalSteps,
				Done:     false,
			}
			if job.optrotate {
				log.Printf("testing rotate")
				if (job.LabelSize.X > job.LabelSize.Y) != (size.X > size.Y) {
					log.Printf("rotating...")
					img = imaging.Rotate90(img)
				}
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "resizing",
				Progress: 3.0 / totalSteps,
				Done:     false,
			}
			if job.optresize {
				log.Printf("resize; stretch: %t", job.optstretch)
				if job.optstretch {
					img = imaging.Resize(img, job.LabelSize.X, job.LabelSize.Y, method)
				} else {
					size = img.Bounds().Size()

					px := float32(size.X) / float32(job.LabelSize.X)
					py := float32(size.Y) / float32(job.LabelSize.Y)

					if px > py {
						img = imaging.Resize(img, job.LabelSize.X, 0, method)
					} else {
						img = imaging.Resize(img, 0, job.LabelSize.Y, method)
					}
				}
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "centering",
				Progress: 4.0 / totalSteps,
				Done:     false,
			}
			if job.optcenterh || job.optcenterv {
				nimg := imaging.New(job.LabelSize.X, job.LabelSize.Y, color.White)
				size = img.Bounds().Size()

				var x, y = 0, 0
				if job.optcenterh {
					x = job.LabelSize.X/2 - size.X/2
				}

				if job.optcenterv {
					y = job.LabelSize.Y/2 - size.Y/2
				}

				draw.Draw(nimg,
					img.Bounds().Add(image.Pt(x, y)),
					img,
					image.Point{},
					draw.Over,
				)

				img = nimg
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "dithering",
				Progress: 5.0 / totalSteps,
				Done:     false,
			}
			if job.ditherer != nil {
				log.Printf("Dithering with %T", job.ditherer)

				img = job.ditherer.Apply(img)
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "saving",
				Progress: 6.0 / totalSteps,
				Done:     false,
			}
			_, err := SaveImage(img, job.UUID.String())
			if err != nil {
				log.Printf("Failed to encode & save image: %s", err)
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "printing",
				Progress: 7.0 / totalSteps,
				Reload:   true,
				Done:     false,
			}

			if !*OptDryRun {
				// PFCount of 0 is no print
				if job.PFCount > 0 {
					err = printer.PrintChunked(img, 0, 0)
					if err != nil {
						imageUpdateCh <- Status{
							UUID:     job.UUID,
							Step:     "Uploading Data: " + err.Error(),
							Progress: -1,
							Done:     true,
						}

						continue
					}

					err = printer.PF(job.PFCount)
					if err != nil {
						imageUpdateCh <- Status{
							UUID:     job.UUID,
							Step:     err.Error(),
							Progress: -1,
							Done:     true,
						}

						continue
					}
				}
			} else {
				conf := GetConfig()
				ctype := T(*PrinterAddressType != "", *PrinterAddressType, conf.PrinterCType)
				log.Printf("Printing %d of size: %+v", job.PFCount, img.Bounds().Size())

				if ctype == "serial" {
					time.Sleep(time.Second * 12)
				}

				if ctype == "net" {
					time.Sleep(time.Second * 5)
				}
			}

			imageUpdateCh <- Status{
				UUID:     job.UUID,
				Step:     "done",
				Progress: 8 / totalSteps,
				Reload:   true,
				Done:     true,
			}
		}
	}
}

func OpenPrinter() *fp.Printer {
	conf := GetConfig()

	host := T(*PrinterAddressHost != "", *PrinterAddressHost, conf.PrinterHost)
	port := T(*PrinterAddressPort != "", *PrinterAddressPort, conf.PrinterPort)

	ctype := T(*PrinterAddressType != "", *PrinterAddressType, conf.PrinterCType)

	var err error
	var p *fp.Printer

	errors := 0
loop:
	for {
		switch ctype {
		case "net":
			log.Printf("Dialing %s", host)
			p, err = fp.DialPrinter(host)
			if err == nil {
				break loop
			}

			log.Printf("Error dialing %s: %s", host, err)

		case "serial":
			log.Printf("Open %s", port)
			p, err = fp.OpenPrinter(port)
			if err == nil {
				break loop
			}

			log.Printf("Error opening %s: %s", port, err)

		default:
			log.Fatalf("Invaid connection type '%s', choose between 'net' and 'serial'", ctype)
		}

		time.Sleep(time.Second * 5)

		errors++
		if errors > 3 {
			log.Fatalf("more than 3 errors, aborting: %s", err)
		}
	}

	if *OptBeep {
		err := p.Beep(fp.Sound{Freq: 850, Dur: 200}, fp.Sound{Freq: 950, Dur: 200})
		if err != nil {
			log.Fatalf("Failed to communicate with printer: Beep: %s", err)
		}
	}

	return p
}
