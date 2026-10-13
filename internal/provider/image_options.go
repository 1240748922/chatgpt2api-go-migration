package provider

import (
 "context"
 "time"
)

type ImageOptions struct {
 StreamTimeout,PollTimeout,PollInterval,InitialWait,Settle time.Duration
 CheckBeforeHit,RemoveConversation bool
}
type imageOptionsKey struct{}
func WithImageOptions(ctx context.Context, options ImageOptions) context.Context {return context.WithValue(ctx,imageOptionsKey{},options)}
func imageOptions(ctx context.Context) ImageOptions {options,_:=ctx.Value(imageOptionsKey{}).(ImageOptions);return options}
func waitImage(ctx context.Context,d time.Duration)error{if d<=0{return ctx.Err()};t:=time.NewTimer(d);defer t.Stop();select{case<-ctx.Done():return ctx.Err();case<-t.C:return nil}}
