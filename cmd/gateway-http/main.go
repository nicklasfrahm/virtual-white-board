package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/nicklasfrahm/showcases/pkg/broker"
	"github.com/nicklasfrahm/showcases/pkg/errs"
	"github.com/nicklasfrahm/showcases/pkg/gateway"
	"github.com/nicklasfrahm/showcases/pkg/service"
)

var (
	name      = "unknown"
	version   = "dev"
	mapMethod = map[string]string{
		http.MethodGet:    "read",
		http.MethodPut:    "update",
		http.MethodDelete: "delete",
	}
	mapListMethod = map[string]string{
		http.MethodGet:  "find",
		http.MethodPost: "create",
	}
)

func main() {
	// Load authorized users.
	users := make(map[string]string)
	usersCreds := strings.Split(os.Getenv("AUTHORIZED_CREDENTIALS"), ",")
	for _, userCred := range usersCreds {
		userPass := strings.Split(userCred, ":")
		if len(userPass) == 2 {
			users[userPass[0]] = userPass[1]
		}
	}

	// Create new service instance.
	svc := service.New(service.Config{
		Name:    name,
		Version: version,
	})

	// Configure broker connection.
	svc.UseBroker(broker.NewNATS(&broker.NATSOptions{
		URI: os.Getenv("BROKER_URI"),
		NATSOptions: []nats.Option{
			nats.Name(name),
			nats.Timeout(1 * time.Second),
			nats.PingInterval(5 * time.Second),
			nats.MaxPingsOutstanding(6),
		},
		RequestTimeout: 20 * time.Millisecond,
	}))

	// Configure gateway.
	svc.UseGateway(gateway.NewHTTP(&gateway.HTTPOptions{
		Port: os.Getenv("PORT"),
	}))

	svc.GatewayEndpoint("/", func(r *service.Request) error {
		// Authorize only special users.
		svc.Logger.Info().Msgf("Middleware: %s", r.Ctx.Request().Header.Peek("Authorization"))
		return r.Ctx.Next()
	})

	// Define endpoint for protocol translation of API v1.
	svc.GatewayEndpoint("/v1", func(r *service.Request) error {
		// TODO: Perform topic normalization in broker implementation.
		// Convert HTTP path to NATS subject.
		subject, err := pathToSubject(r.Ctx.Method(), r.Ctx.Path())
		if err != nil {
			return err
		}

		// Parse body.
		var body interface{}
		if r.Ctx.Method() == http.MethodPost || r.Ctx.Method() == http.MethodPut {
			if err := r.Ctx.BodyParser(&body); err != nil {
				svc.Logger.Warn().Msg(err.Error())
				return err
			}
		}

		event := cloudevents.NewEvent()
		event.SetID(uuid.NewString())
		event.SetSource("gateway-http")
		event.SetType(subject)
		event.SetDataContentType(cloudevents.ApplicationJSON)
		event.SetData(body)

		// Encode cloud event.
		encodedEvent, err := json.Marshal(event)
		if err != nil {
			return err
		}

		msg, err := svc.Broker.Request(subject, encodedEvent)
		if err != nil {
			return errs.InvalidService
		}

		// TODO: Set HTTP status based on service response.
		r.Ctx.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSONCharsetUTF8)
		return r.Ctx.Send(*msg.Data)
	})

	// Wait until error occurs or signal is received.
	svc.Start()
}

// TODO: Test for edge cases, such as dots in the path.
func pathToSubject(method string, path string) (string, error) {
	// Convert resource path to NATS subject.
	versionedResourceSubject := strings.ReplaceAll(path[1:], "/", ".")

	// Check if the path describes a specific resource or a resource list.
	// This assumes that thescheme is: /:version/resource/:rid/subresource/:srid.
	if strings.Count(versionedResourceSubject, ".")%2 != 0 {
		// Resource lists do not support PUT or DELETE methods.
		if method == http.MethodPut || method == http.MethodDelete {
			return "", errs.InvalidEndpoint
		}
		return fmt.Sprintf("%s.%s", versionedResourceSubject, mapListMethod[method]), nil
	}

	// Specific resources do not support the POST method.
	if method == http.MethodPost {
		return "", errs.InvalidEndpoint
	}
	return mapMethod[method], nil
}
