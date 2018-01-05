# CloudFormation

Create Stack:
```
aws --region ap-southeast-2 cloudformation create-stack --stack-name distributed --template-body file://distributed.yaml --parameters ParameterKey=KeyName,ParameterValue=[KeyName] --capabilities CAPABILITY_NAMED_IAM
```

Update Stack:
```
aws --region ap-southeast-2 cloudformation update-stack --stack-name distributed --template-body file://distributed.yaml --parameters ParameterKey=KeyName,ParameterValue=[KeyName] --capabilities CAPABILITY_NAMED_IAM
```

Delete Stack:
```
aws --region ap-southeast-2 cloudformation delete-stack --stack-name distributed
```

Describe Stack:
```
aws --region ap-southeast-2 cloudformation describe-stacks --stack-name distributed
```
