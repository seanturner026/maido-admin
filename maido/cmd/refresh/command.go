package refresh

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var Command = &cobra.Command{
	Use:          "refresh",
	Short:        "Updates the DynamoDB backend.",
	SilenceUsage: true,
	Run: func(cmd *cobra.Command, args []string) {
		main()
	},
}

func initialize_clients() (*dynamodb.Client, *s3.Client, error) {
	viper.SetDefault("awsEndpoint", "")
	awsEndpoint := viper.GetString("aws_test_endpoint")
	// TODO: Get working
	customResolver := aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
		return aws.Endpoint{
			PartitionID:   "aws",
			URL:           awsEndpoint,
			SigningRegion: "us-east-1",
		}, nil
	})

	var err error
	var cfg aws.Config

	if awsEndpoint == "" {
		cfg, err = config.LoadDefaultConfig(context.TODO())
	} else {
		cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithEndpointResolver(customResolver))
	}

	if err != nil {
		return nil, nil, fmt.Errorf("failed to load AWS configuration %v", err)
	}

	client_dynamodb := dynamodb.NewFromConfig(cfg)
	client_s3 := s3.NewFromConfig(cfg)
	return client_dynamodb, client_s3, nil
}

type items struct {
	Items []item
}

type item struct {
	Name        string `dynamodbav:"PK"          json:"name"`
	SK          string `dynamodbav:"SK"          json:",omitempty"`
	Status      string `dynamodbav:"DK1"         json:"status"`
	Description string `dynamodbav:"Description" json:"description"`
	Price       string `dynamodbav:"Price"       json:"price"`
}

func obtainLatestItems(inventoryLocation string) (string, error) {
	inventoryContents, err := os.ReadDir(inventoryLocation)
	if err != nil {
		return "", fmt.Errorf("could not read inventories directory %v", err)
	}
	item := inventoryContents[len(inventoryContents)-1]
	return fmt.Sprintf("%s/%s", inventoryLocation, item.Name()), nil
}

func readItems(inventoryFileName string) (*items, error) {
	items := &items{}
	content, err := os.ReadFile(inventoryFileName)
	if err != nil {
		return items, fmt.Errorf("error reading inventory file %s, %v", inventoryFileName, err)
	}

	err = json.Unmarshal(content, items)
	if err != nil {
		return items, fmt.Errorf("error unmarshaling inventory, %v", err)
	}
	return items, nil
}

func generatePutRequestInput(item item) (map[string]types.AttributeValue, error) {
	item.SK = "PRODUCT<>BLEND"
	putItemInput, err := attributevalue.MarshalMap(item)
	if err != nil {
		return map[string]types.AttributeValue{}, err
	}
	return putItemInput, err
}

func generateBatchWriteItemInputs(items items, tableName string) ([]*dynamodb.BatchWriteItemInput, error) {
	inputs := []*dynamodb.BatchWriteItemInput{}
	input := &dynamodb.BatchWriteItemInput{
		RequestItems: map[string][]types.WriteRequest{
			tableName: {},
		},
	}

	for i, putItem := range items.Items {
		if (i+1)%25 == 0 || i+1 == len(items.Items) {
			inputs = append(inputs, input)
			if (i+1)%25 == 0 {
				input = &dynamodb.BatchWriteItemInput{
					RequestItems: map[string][]types.WriteRequest{
						tableName: {},
					},
				}
			}
		}

		putItemInput, err := generatePutRequestInput(putItem)
		if err != nil {
			return []*dynamodb.BatchWriteItemInput{}, fmt.Errorf(
				"error generating batch write item input for %s, %v",
				putItem.Name,
				err,
			)
		}

		putItemRequest := types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: putItemInput,
			},
		}
		input.RequestItems[tableName] = append(input.RequestItems[tableName], putItemRequest)
	}
	return inputs, nil
}

type writeErrorChannel struct {
	Service string
	Error   error
}

func orchestrateWrites(
	client_dynamodb *dynamodb.Client,
	client_s3 *s3.Client,
	inputs []*dynamodb.BatchWriteItemInput,
	tableName string,
) error {

	requestCount := len(inputs)
	if len(inputs) > 1 {
		requestCount += (requestCount-1)*25 + len(inputs[requestCount-1].RequestItems[tableName])
	} else {
		requestCount += len(inputs[requestCount-1].RequestItems[tableName])
	}

	writeChan := make(chan writeErrorChannel, requestCount)
	wg := new(sync.WaitGroup)
	wg.Add(requestCount)
	bucketName := viper.GetString("bucket_name")
	var objectName string
	for _, input := range inputs {
		go writeItemsToDynamoDB(client_dynamodb, tableName, input, wg, writeChan)

		for _, requestItem := range input.RequestItems[tableName] {
			objectName = fmt.Sprintf("%s.png", requestItem.PutRequest.Item["PK"].(*types.AttributeValueMemberS).Value)
			go writeItemToS3(
				client_s3,
				bucketName,
				objectName,
				wg,
				writeChan,
			)
		}
	}

	wg.Wait()
	close(writeChan)
	if len(writeChan) > 0 {
		for err := range writeChan {
			log.Printf("Error writing objects to %s, %+v", err.Service, err.Error)
		}
		return fmt.Errorf("error writing objects")
	}
	return nil
}

func writeItemsToDynamoDB(
	client *dynamodb.Client,
	tableName string,
	input *dynamodb.BatchWriteItemInput,
	wg *sync.WaitGroup,
	ch chan writeErrorChannel,
) {
	defer wg.Done()

	resp, err := client.BatchWriteItem(context.TODO(), input)
	if err != nil {
		res := writeErrorChannel{Error: err, Service: "DynamoDB"}
		ch <- res
		return
	}

	_, ok := resp.UnprocessedItems[tableName]
	if ok {
		for {
			log.Print("Unprocessed items remain. Processing...")
			input = &dynamodb.BatchWriteItemInput{
				RequestItems: resp.UnprocessedItems,
			}
			resp, err = client.BatchWriteItem(context.TODO(), input)
			if err != nil {
				log.Printf("Error while processing unprocessed items %v", err)
			}
			if resp.UnprocessedItems == nil {
				break
			}
		}
	}
}

// TODO: Implement checks
// Check the item is in assets directory
// Check the item doesn't exist in S3 currently
func writeItemToS3(client *s3.Client, bucketName, objectName string, wg *sync.WaitGroup, ch chan writeErrorChannel) {
	defer wg.Done()
	image, err := os.Open(fmt.Sprintf("./assets/%s", objectName))
	if err != nil {
		res := writeErrorChannel{Error: err, Service: "S3"}
		ch <- res
		return
	}
	defer image.Close()

	input := &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(objectName),
		Body:   image,
	}

	_, err = client.PutObject(context.TODO(), input)
	if err != nil {
		res := writeErrorChannel{Error: err, Service: "S3"}
		ch <- res
		return
	}
}

func main() {
	client_dynamodb, client_s3, err := initialize_clients()
	if err != nil {
		log.Fatal(err)
	}

	inventory, err := obtainLatestItems("./inventories")
	if err != nil {
		log.Fatal(err)
	}

	items, err := readItems(inventory)
	if err != nil {
		log.Fatal(err)
	}

	tableName := viper.GetString("table_name")
	inputs, err := generateBatchWriteItemInputs(*items, tableName)
	if err != nil {
		log.Fatal(err)
	}

	orchestrateWrites(client_dynamodb, client_s3, inputs, tableName)
}
