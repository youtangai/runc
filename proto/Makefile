NAME := service

$(NAME).pb.go: $(NAME).proto
	protoc -I ./ --go_out=plugins=grpc:./ ./$(NAME).proto
