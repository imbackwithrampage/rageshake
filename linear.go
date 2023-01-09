package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	linearTeamAndroid = "39dc6884-3753-4b41-ad19-a166a0f2f51d"
	linearTeamiOS     = "6d2402bc-d4bc-4d3d-8f5e-96df51cafe22"
	linearTeamDesktop = "4c83bd23-2236-40b5-a250-88bbc8cc446a"
	linearTeamBridges = "a5b96b19-c49e-4f2a-8372-206eefeba471"
)

const (
	problemSignIn         = "Trouble connecting Beeper to a chat network"
	problemSend           = "I can't send a message"
	problemReceiveAny     = "I can't receive any messages"
	problemReceiveCertain = "I can't receive certain messages"
	problemUI             = "Problem with app buttons/interface/text"
	problemEncryption     = "Encryption/decryption error"
	problemNotifications  = "Notifications problem"
	problemFeatureRequest = "Feature request"
	problemBridgeRequest  = "Bridge Request"
	problemOther          = "Other"
)

var appToTeamID = map[string]string{
	"beeper-android": linearTeamAndroid,
	"beeper-ios":     linearTeamiOS,
	"beeper-desktop": linearTeamDesktop,
}

const (
	labelRageshake        = "3fc786e7-b4f1-472e-8e27-4aa97c2eb27c"
	labelSupportReview    = "f1d19cb7-0839-4349-aa9a-f5eaec84a3a2"
	labelHungryUser       = "b213456c-be3a-4838-8849-f009a0b92c52"
	labelLegacyUser       = "3a9a5403-339f-48ec-87e1-e472dfbefeea"
	labelHungryiMCUser    = "94dfba99-7b65-4542-8dfd-c6df716361ea"
	labelNonClusterHungry = "da3376cb-9ac4-44cb-8611-a5eb57656198"
)

var problemToLabelID = map[string]string{
	problemSend:           "02805b84-e966-49ee-8c8b-ac5b3350a9e4",
	problemReceiveAny:     "140462f3-1ef2-4bad-a540-b3ee38a6a654",
	problemReceiveCertain: "f3574891-6854-46ba-b82e-b695bcbdf613",
	problemSignIn:         "14ca00de-66e3-4b18-a855-6ff86841e0e6",
	problemUI:             "b58efad1-22ed-4c88-99b2-e7d99f9c8556",
	problemEncryption:     "e57ee874-2924-4eea-9c64-57d60c478653",
	problemNotifications:  "2f562c13-2a64-44f8-a580-dd175cc4b6f5",
	problemFeatureRequest: "32c7fb7d-a155-4857-9333-2c203e7b731f",
	problemBridgeRequest:  "eed94025-eae7-4e02-9abf-870519f7369b",
	problemOther:          "0b40c728-66af-4ca9-b1fb-62c0bcda81ba",
}

var bridgeToLabelID = map[string]string{
	"android-sms":    "23b9c42b-eb91-424a-9810-181748f98543",
	"androidsms":     "23b9c42b-eb91-424a-9810-181748f98543",
	"discord":        "e5191313-88e1-4a9d-b5ed-b67a9b2de861",
	"discordgo":      "6ce5f0c2-13ec-48ca-b4d8-e3f170fcfb8c",
	"facebook":       "076cce46-9efb-463d-9cce-3726945091d9",
	"googlechat":     "f2fcfb8e-15ba-41f0-bd7e-6080660aa4fc",
	"imessage-cloud": "10ac3928-b657-409d-a1eb-4f9ec7df870e",
	"imessagecloud":  "10ac3928-b657-409d-a1eb-4f9ec7df870e",
	"imessage":       "e0f45fd9-a8ed-43db-8866-d79ef8622ab2",
	"instagram":      "e4b3fa54-c9da-462e-a680-6946fd5ba1fb",
	"linkedin":       "d0d8b87b-6058-4093-946a-b395f31aba1e",
	"signal":         "8ea186ae-c3da-4c57-b50e-b5b82d2c32f0",
	"slack":          "3c692f5a-dd73-4969-ac03-fc2ec15abd95",
	"slackgo":        "306ca483-10e8-4da3-b24b-e7696466e5a9",
	"telegram":       "95089bee-0341-4363-bdf0-d420c968bb73",
	"twitter":        "35f6be99-f9f0-480e-b3e9-be29e74fa8cf",
	"whatsapp":       "efd1d28a-5188-4ab3-9a27-51a63f9c7a16",
}

type GraphQLRequest struct {
	Token     string                 `json:"-"`
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type GraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		UserPresentableMessage string `json:"userPresentableMessage"`
	}
}

type GraphQLResponse struct {
	Errors []GraphQLError
	Data   json.RawMessage
}

type CreateIssueResponse struct {
	IssueCreate struct {
		Success bool
		Issue   struct {
			ID         string
			Title      string
			Identifier string
			URL        string
		}
	}
}

const mutationCreateIssue = `
mutation CreateIssue($input: IssueCreateInput!) {
    issueCreate(input: $input) {
        success
        issue {
            id
            title
            identifier
            url
        }
    }
}
`

type UserEmail struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type GetUserEmailsResponse struct {
	Users struct {
		Nodes []UserEmail `json:"nodes"`
	} `json:"users"`
}

// This will start missing users if we have more than 250 active linear accounts
const queryGetUserEmails = `
query {
	users(first: 250) {
		nodes {
			id
			name
			email
		}
	}
}
`

const queryFindUserByEmail = `
query FindUserByEmail(filter: UserFilter!) {
	users(filter: $filter) {
		nodes {
			id
			name
			email
		}
	}
}
`

var emailToLinearIDCache = make(map[string]string)

func getLinearID(email, token string) string {
	// Ensure there's only one @ and the domain is beeper.com
	parts := strings.Split(email, "@")
	if len(parts) != 2 || parts[1] != "beeper.com" {
		return ""
	}
	// Remove anything after a +
	parts = strings.Split(parts[0], "+")
	email = fmt.Sprintf("%s@beeper.com", parts[0])
	userID, ok := emailToLinearIDCache[email]
	if ok {
		return userID
	}
	fmt.Println("ID for", email, "not cached, fetching from Linear")
	var userResp GetUserEmailsResponse
	err := LinearRequest(&GraphQLRequest{
		Token: token,
		Query: queryFindUserByEmail,
		Variables: map[string]any{
			"filter": map[string]any{"email": map[string]any{"eq": email}},
		},
	}, &userResp)
	if err != nil {
		fmt.Printf("Error finding linear ID of %s: %v\n", email, err)
		emailToLinearIDCache[email] = ""
		return ""
	}
	for _, user := range userResp.Users.Nodes {
		fmt.Printf("Found linear ID for %s (%s) -> %s\n", user.Email, user.Name, user.ID)
		emailToLinearIDCache[user.Email] = user.ID
	}
	return emailToLinearIDCache[email]
}

func fillEmailCache(token string) error {
	var userResp GetUserEmailsResponse
	err := LinearRequest(&GraphQLRequest{
		Token: token,
		Query: queryGetUserEmails,
	}, &userResp)
	if err != nil {
		return err
	}
	for _, user := range userResp.Users.Nodes {
		fmt.Printf("Found linear ID for %s (%s) -> %s\n", user.Email, user.Name, user.ID)
		emailToLinearIDCache[user.Email] = user.ID
	}
	return nil
}

func LinearRequest(payload *GraphQLRequest, into interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(payload)
	if err != nil {
		return fmt.Errorf("failed to encode request JSON: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", &buf)
	if err != nil {
		return fmt.Errorf("failed to create GraphQL request: %w", err)
	}
	req.Header.Add("Authorization", payload.Token)
	req.Header.Add("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send GraphQL request: %w", err)
	}
	defer resp.Body.Close()
	var respData GraphQLResponse
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("Got non-200 response %d: %s", resp.StatusCode, data)
	}
	fmt.Printf("%s\n", data)
	err = json.Unmarshal(data, &respData)
	//err = json.NewDecoder(resp.Body).Decode(&respData)
	if err != nil {
		return fmt.Errorf("failed to unmarshal response JSON (status %d): %w: %s", resp.StatusCode, err, data)
	}
	if len(respData.Errors) > 0 {
		if len(respData.Errors[0].Extensions.UserPresentableMessage) > 0 {
			return fmt.Errorf("GraphQL error: %s", respData.Errors[0].Extensions.UserPresentableMessage)
		}
		return fmt.Errorf("GraphQL error: %s", respData.Errors[0].Message)
	}
	if into != nil {
		err = json.Unmarshal(respData.Data, &into)
		if err != nil {
			return fmt.Errorf("failed to unmarshal response data: %w", err)
		}
	}
	return nil
}
