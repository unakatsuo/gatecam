package main

import (
	"flag"
	"fmt"
	"image"
	"io/ioutil"
	"log"
	"math"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/rekognition"
	"gocv.io/x/gocv"
)

var settings struct {
	WebCamDevID                string
	AwsRegion                  string
	AwsAccessKeyID             string
	AwsSecretAccessKey         string
	AwsRekognitionCollectionID string
}

func init() {
	flag.StringVar(&settings.WebCamDevID, "device", "0", "Web camera device ID")
	flag.StringVar(&settings.AwsRegion, "aws-region", "us-east-1", "AWS Region")
	flag.StringVar(&settings.AwsRekognitionCollectionID, "aws-collection-id", "", "AWS Rekognition Collection ID")

	settings.AwsAccessKeyID = os.Getenv("AWS_ACCESS_KEY")
	settings.AwsSecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
}

func main() {
	flag.Parse()
	if settings.AwsRekognitionCollectionID == "" {
		log.Print("-aws-collection-id is unset")
		return
	}

	// open webcam
	webcam, err := gocv.OpenVideoCapture(settings.WebCamDevID)
	if err != nil {
		log.Print("error opening video capture device: ", settings.WebCamDevID)
		return
	}
	defer webcam.Close()
	webcam.Set(gocv.VideoCaptureFrameWidth, 640.0)
	webcam.Set(gocv.VideoCaptureFrameHeight, 480.0)
	log.Printf("capure depth: %d x %d", int(webcam.Get(gocv.VideoCaptureFrameWidth)), int(webcam.Get(gocv.VideoCaptureFrameHeight)))

	// Discard first number of frames until the camera stabilizes brightness.
	webcam.Grab(10)
	for {
		capture(webcam)
	}

}

func capture(webcam *gocv.VideoCapture) {
	frame := gocv.NewMat()
	defer frame.Close()

	if ok := webcam.Read(&frame); !ok {
		log.Print("Device closed: ", settings.WebCamDevID)
		return
	}
	if frame.Empty() {
		log.Print("empty frame")
		return
	}

	jpegBytes, err := gocv.IMEncode(gocv.JPEGFileExt, frame)
	if err != nil {
		log.Print(err)
		return
	}
	ioutil.WriteFile("capture.jpg", jpegBytes, 0644)

	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(settings.AwsRegion),
		Credentials: credentials.NewStaticCredentials(settings.AwsAccessKeyID, settings.AwsSecretAccessKey, ""),
	}))

	imageInput := &rekognition.Image{
		Bytes: jpegBytes,
	}

	reko := rekognition.New(sess)

	faceArea, err := func() ([]image.Rectangle, error) {

		input := &rekognition.DetectFacesInput{
			Image: imageInput,
		}

		output, err := reko.DetectFaces(input)
		if err != nil {
			log.Println(err)
			return nil, err
		}

		res := make([]image.Rectangle, len(output.FaceDetails))
		log.Print(output)
		for idx, f := range output.FaceDetails {
			// DetectFaces API can return the bounding box value in out of image dimension.
			// They needs to be capped from 0.0 to 1.0.
			rect := image.Rect(
				int(math.Max(*f.BoundingBox.Left*float64(frame.Cols()), 0.0)),
				int(math.Max(*f.BoundingBox.Top*float64(frame.Rows()), 0.0)),
				int(math.Min(*f.BoundingBox.Left+*f.BoundingBox.Width, 1.0)*float64(frame.Cols())),
				int(math.Min(*f.BoundingBox.Top+*f.BoundingBox.Height, 1.0)*float64(frame.Rows())),
			)

			res[idx] = rect
		}
		return res, nil
	}()
	if err != nil {
		return
	}

	identify := func(jpegBytes []byte) error {

		imageInput := &rekognition.Image{
			Bytes: jpegBytes,
		}
		input := &rekognition.SearchFacesByImageInput{
			CollectionId: aws.String(settings.AwsRekognitionCollectionID),
			Image:        imageInput,
		}
		output, err := reko.SearchFacesByImage(input)
		if err != nil {
			log.Println(err)
			return err
		}

		log.Print("SeachFaceByImage result: ", len(output.FaceMatches))

		for _, f := range output.FaceMatches {
			if f.Face.ExternalImageId == nil {
				log.Print("Found guest: face_id=", *f.Face.FaceId)
			} else {
				log.Print("Identified ", *f.Face.ExternalImageId)
			}
		}
		return nil
	}

	for idx, r := range faceArea {
		func() error {
			cropped := frame.Region(r)
			defer cropped.Close()

			jpegBytes, err := gocv.IMEncode(gocv.JPEGFileExt, cropped)
			if err != nil {
				log.Print(err)
				return err
			}
			identify(jpegBytes)
			ioutil.WriteFile(fmt.Sprintf("guest%d.jpg", idx), jpegBytes, 0644)
			return nil
		}()
	}

}
