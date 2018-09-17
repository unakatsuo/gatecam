package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"io/ioutil"
	"log"
	"math"
	"os"
	"sort"
	"strings"

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

	store := &LocalStore{baseDir: "localstore"}
	if err := store.Setup(); err != nil {
		log.Printf("%T: %s", store, err)
		return
	}

	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(settings.AwsRegion),
		Credentials: credentials.NewStaticCredentials(settings.AwsAccessKeyID, settings.AwsSecretAccessKey, ""),
	}))

	if err := watch(store, sess); err != nil {
		log.Print("watch: ", err)
		return
	}

	// Discard first number of frames until the camera stabilizes brightness.
	webcam.Grab(10)
	for {
		capture(webcam, store, sess)
	}

}

func capture(webcam *gocv.VideoCapture, store Store, sess *session.Session) {
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

	identify := func(jpegBytes []byte, idx int) error {

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

		if len(output.FaceMatches) == 0 {
			if err := store.SaveGuest(jpegBytes, idx); err != nil {
				log.Printf("%T: %s", store, err)
			}
			return nil
		}

		results := []faceSimilarity{}
		for _, f := range output.FaceMatches {
			if f.Face.ExternalImageId == nil {
				log.Print("Found but no exterImageId attribute: face_id=", *f.Face.FaceId)
				continue
			}
			k, err := ParseFaceKey(*f.Face.ExternalImageId)
			if err != nil {
				continue
			}
			results = append(results, faceSimilarity{k, *f.Similarity})
		}
		if len(results) == 0 {
			return nil
		}
		sort.Sort(bySimilarity(results))
		log.Print("Identified: ", results[0].Key.Name)

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
			identify(jpegBytes, idx)
			return nil
		}()
	}

}

type faceSimilarity struct {
	Key        FaceKey
	Similarity float64
}
type bySimilarity []faceSimilarity

func (c bySimilarity) Len() int {
	return len(c)
}

func (c bySimilarity) Less(i, j int) bool {
	return c[i].Similarity < c[j].Similarity
}

func (c bySimilarity) Swap(i, j int) {
	c[i], c[j] = c[j], c[i]
}

type FaceKey struct {
	Name  string
	Index string
}

func ParseFaceKey(s string) (FaceKey, error) {
	i := strings.SplitN(s, "_", 2)
	if len(i) != 2 {
		return FaceKey{}, errors.New("Invalid face key syntax")
	}
	return FaceKey{i[0], i[1]}, nil
}

type SyncFunc func([]FaceKey) error

type Store interface {
	Setup() error
	SaveGuest(img []byte, idx int) error
	Watch(synccb SyncFunc) error
	ReadImage(key FaceKey) ([]byte, error)
}

func watch(store Store, sess *session.Session) error {

	synccb := func(locals []FaceKey) error {
		reko := rekognition.New(sess)

		input := &rekognition.ListFacesInput{
			CollectionId: aws.String(settings.AwsRekognitionCollectionID),
		}
		output, err := reko.ListFaces(input)
		if err != nil {
			log.Print(err)
			return err
		}

		registered := [][2]string{}
		for _, f := range output.Faces {
			if f.ExternalImageId == nil {
				continue
			}
			i := strings.SplitN(*f.ExternalImageId, "_", 2)
			if len(i) != 2 {
				log.Print("Skipped externalImageId: ", *f.ExternalImageId)
				continue
			}

			registered = append(registered, [2]string{i[0], i[1]})
		}

		newkeys := []FaceKey{}
		for _, k := range locals {
			func() {
				for _, k2 := range registered {
					if k.Name == k2[0] && k.Index == k2[1] {
						return
					}
				}
				newkeys = append(newkeys, k)
			}()
		}

		for _, k := range newkeys {
			jpegBytes, err := store.ReadImage(k)
			if err != nil {
				log.Print("store.ReadImage: ", err)
				continue
			}
			input := &rekognition.IndexFacesInput{
				CollectionId:    aws.String(settings.AwsRekognitionCollectionID),
				ExternalImageId: aws.String(fmt.Sprintf("%s_%s", k.Name, k.Index)),
				Image: &rekognition.Image{
					Bytes: jpegBytes,
				},
			}
			_, err = reko.IndexFaces(input)
			if err != nil {
				log.Print("rekognition.IndexFaces: ", err)
			}
			log.Print("Indexed new face: ", k)
		}
		return nil
	}

	if err := store.Watch(synccb); err != nil {
		return err
	}
	return nil
}
