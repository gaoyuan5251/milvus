// Code generated by mockery v2.46.0. DO NOT EDIT.

package mock_interceptors

import (
	interceptors "github.com/milvus-io/milvus/internal/streamingnode/server/wal/interceptors"
	mock "github.com/stretchr/testify/mock"
)

// MockInterceptorBuilder is an autogenerated mock type for the InterceptorBuilder type
type MockInterceptorBuilder struct {
	mock.Mock
}

type MockInterceptorBuilder_Expecter struct {
	mock *mock.Mock
}

func (_m *MockInterceptorBuilder) EXPECT() *MockInterceptorBuilder_Expecter {
	return &MockInterceptorBuilder_Expecter{mock: &_m.Mock}
}

// Build provides a mock function with given fields: param
func (_m *MockInterceptorBuilder) Build(param interceptors.InterceptorBuildParam) interceptors.Interceptor {
	ret := _m.Called(param)

	if len(ret) == 0 {
		panic("no return value specified for Build")
	}

	var r0 interceptors.Interceptor
	if rf, ok := ret.Get(0).(func(interceptors.InterceptorBuildParam) interceptors.Interceptor); ok {
		r0 = rf(param)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(interceptors.Interceptor)
		}
	}

	return r0
}

// MockInterceptorBuilder_Build_Call is a *mock.Call that shadows Run/Return methods with type explicit version for method 'Build'
type MockInterceptorBuilder_Build_Call struct {
	*mock.Call
}

// Build is a helper method to define mock.On call
//   - param interceptors.InterceptorBuildParam
func (_e *MockInterceptorBuilder_Expecter) Build(param interface{}) *MockInterceptorBuilder_Build_Call {
	return &MockInterceptorBuilder_Build_Call{Call: _e.mock.On("Build", param)}
}

func (_c *MockInterceptorBuilder_Build_Call) Run(run func(param interceptors.InterceptorBuildParam)) *MockInterceptorBuilder_Build_Call {
	_c.Call.Run(func(args mock.Arguments) {
		run(args[0].(interceptors.InterceptorBuildParam))
	})
	return _c
}

func (_c *MockInterceptorBuilder_Build_Call) Return(_a0 interceptors.Interceptor) *MockInterceptorBuilder_Build_Call {
	_c.Call.Return(_a0)
	return _c
}

func (_c *MockInterceptorBuilder_Build_Call) RunAndReturn(run func(interceptors.InterceptorBuildParam) interceptors.Interceptor) *MockInterceptorBuilder_Build_Call {
	_c.Call.Return(run)
	return _c
}

// NewMockInterceptorBuilder creates a new instance of MockInterceptorBuilder. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
// The first argument is typically a *testing.T value.
func NewMockInterceptorBuilder(t interface {
	mock.TestingT
	Cleanup(func())
}) *MockInterceptorBuilder {
	mock := &MockInterceptorBuilder{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}